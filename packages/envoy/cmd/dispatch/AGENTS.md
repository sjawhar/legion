# Dispatch HTTP Server (Go)

`cmd/dispatch` serves the Dispatch dashboard, native API, document rooms, Google
sign-in through the sign-in pool, and the read-only GitHub proxy the web app's
GitHub reads go through as the GitHub App.

## Startup and persistence

`DATABASE_URL` and `DISPATCH_AGENT_TOKEN` are required. Startup opens the shared
`pgxpool.Pool` at `store.sharedPoolSize` connections — the size is a property of
Dispatch rather than of the URL or the task's CPU allotment, and a `DATABASE_URL`
carrying `pool_max_conns` is refused at open rather than silently overridden —
applies embedded migrations from `internal/dispatch/store/migrations` (refusing
the whole set, applying nothing, when `pgmigrate.Load` refuses it, and bounding
every migration's lock waits at `pgmigrate.LockTimeout`; README "Database
migrations"), then binds its port, registers the process in the registry of
live server processes (`internal/dispatch/processes`, migration `0135`: a key
set a live process does not share both ways refuses the start, exiting 1
before anything is served, once the process has waited up to one live window
(15 s) and a heartbeat for the refusing process to stop heartbeating, so a
restart after a kill starts once the dead process's row lapses, and a SIGTERM
during that wait logs `dispatch: stopped while waiting for admission to the
registry of live server processes` at INFO and exits 0; `joinRegistry` in
`registry.go`), and starts HTTP serving; shutdown deregisters it
once HTTP has drained. A process whose heartbeat lapsed (frozen, cut from the
database) or whose row was pruned checks its keys again before it counts as
live once more; beside a live process whose keys it does not share, its
heartbeat refuses (`processes.Registration.Lost`), and `serveUntilStopped`
logs `dispatch: this server process no longer shares its signing keys with a
live server process` at ERROR, stops serving as at SIGTERM (listener closed,
requests in flight answered, row deleted) and returns the refusal, so the
process exits 1. The Postgres store contains people,
native issues, artifacts, document updates, and the event outbox.
A migration must leave the release before it able to read and write the
schema, since two processes serve one database through a rolling update: a
change expands in one release and contracts in the next, and one that cannot
be written that way is deployed with no server of the earlier release running
(README "Database migrations" and "Running more than one process";
`internal/dispatch/store/README.md`).
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
generated from it. A variable a release stopped reading leaves the table for
`removedSettings`, beside it, with what replaced it: the table reads it too, and
`resolveBootConfig` refuses to start while it is set (`refuseRemovedSettings`).

`dispatchHandler` mounts the one `GET /healthz` the process serves on its own
mux, above the dashboard router. It is a readiness probe and never a liveness
check: its status is `200` while this process can serve requests and `503`
while the database does not answer, so a liveness probe on it would restart
every server process during a database failover. The probe reads the database through
`store.Pool.Healthy` — a one-connection pool nothing else uses, under
`store.healthProbeTimeout`, two seconds covering dial and query. It never waits
on the shared pool: a busy period legitimately leaves that pool with no free
connection, and a probe queued behind a writer reads as a dead process, so the
ALB fails it at five seconds and ECS replaces the task, cancelling every
in-flight request of every other client. The compose healthcheck and the deploy
script are tighter still at three seconds. The deadline covers the other half —
a database that drops packets rather than refusing is answered `503` with
`db: false` inside the bound, never with silence, and the reason is logged.
`ok` is `db`: `nats` (the publisher's connection, `null` with NATS disabled;
`serverBus`'s pings declare a silently cut connection lost within two to three
seconds), `files`, `signin` and `service_tokens` (each issuer's discovery
document read, `null` when that issuer is not configured) are reported and
none decides `ok` or the status, since the process serves through an outage of
each; a check that needs one asserts it itself (`healthzHandler`).

Beside those, `/healthz` names what is deployed: `commit`, the
legion commit this process's image build stamped (`main.buildCommit`, from the Dockerfile's
`LEGION_COMMIT` build argument, which the release workflow sets to its
`github.sha` and which must be a full sha or empty; `null` in an unstamped
build); `fleet_commit`, the commit of the latest announcement that every live
server process serves one build (`startup.Announcer.FleetCommit`, the one
`server.started` carries; `null` when there is none or before this process's
announcer first read it, and up to one five-second announcer pass behind the
table), which the Legion daemon reads; and `schema_version`, `max(version)` from `schema_migrations`, read by
the same probe query (`null` whenever `db` is false). The deployment repository's
dispatch-apply lane compares `commit` and `schema_version` with the image pin it applied and that commit's
migrations.

Migration 0010 added stored generated `search` columns. Migrations `0057`–`0061` made them plain
columns whose `BEFORE INSERT OR UPDATE` triggers maintain `search_text`-normalised vectors, so no
application code writes or refreshes them. Each trigger builds its vector with `search_vector`
(0087, the issues trigger's from 0088): the whole text where its vector fits Postgres's limit on
one tsvector, and otherwise the longest of its first half, quarter, eighth, … that fits, cut
between two words (`README.md` "Search"). The issues trigger also writes `issues.title_lexemes`
(0088) with `title_lexemes(title)`, the one definition of a title's lexemes, which the
duplicate-title check reads for every stored title and calls for the new one.

Meaning search embeds through `embed.Embedder`, which `newEmbedder` (`cmd/dispatch/embedder.go`)
builds from `DISPATCH_EMBEDDER`: `embed.NewBedrock` (the default; no AWS region is a warning and
keyword-only search) or `embed.NewAzure` (the `DISPATCH_AZURE_*` rows, which `resolveEmbedder`
checks and refuses by name before anything is called, so a model service or Entra that is down
never refuses boot). Each client owns its own numbers (`Limits`: batch size, the most bytes of
text one background call carries so it fits the place's request timeout (320,000 on Azure, none on
Bedrock; `embedqueue`'s `chunkForBudget` sizes each call to it), and the background token pace)
and its identity (`Identity`): `us.cohere.embed-v4:0` on Bedrock, `azure:<endpoint
host>/<deployment>` on Azure. `embeddings.model` records the identity of the client that made
each vector, `api/search.go` compares a query vector only with stored vectors of
its own identity, and `embedqueue.Run` runs the coverage pass (`embedqueue/coverage.go`, `Cover`)
once at start: it embeds what no trigger enqueued, repairs a row whose text changed with the
triggers off, settles a dead row with nothing to embed, and, once the configured embedder has
answered, drops every vector another identity made and re-admits a row dead under another
identity. Answered means an Embed by this process's queue has succeeded (`Deps.proven`, which
`Run` shares between its two loops) or, when none has and such rows exist (one `exists` query, so a
pass with none calls the embedder zero times), an `embedCanary` embed the pass sends succeeds; a
failed or throttled canary leaves those rows in place and logs one WARN with the identity, the
error and the counts left. After the boot pass only those identity steps run again
(`runCoverage`): at once when the queue's first embed proves an embedder the pass's canary could
not, and every `identityRecheckInterval` (10 minutes, jittered) while a run still finds another
identity's rows, so a vector an old-configuration process commits during a roll is dropped; once a
run finds none, the loop holds no timer. `coverageSources` there is the one list of what meaning
search covers, read by the pass and by `GET /api/v1/search/coverage` (`embedqueue.Coverage`); its
texts are the triggers' (0073-0077; an issue's, since 0140, its title and its spec, the latest
version of its primary document, which a second trigger on `artifact_versions` re-queues on a spec
edit) byte for byte, so a kind added to search changes both. There is no backfill subcommand: the
boot pass is the only catch-up, and it takes no lease, so every process of a deployment takes the
same embedder settings (README "Search").

`DISPATCH_REPO_PROJECTS` optionally seeds repository-to-project settings at boot
with comma-separated `owner/repo=KEY` entries (`api.SeedRepoProjects`). Stored dashboard mappings are
authoritative: the seed adds only a repository with no mapping, under the same locks as a mapping
`PUT`, so a seed racing a `PUT` of that repository on another instance waits for it, and each
mapping it adds emits `settings.repo_project.updated` on its project, actor the `environment`
session. External issues fall back to `DISPATCH_DEFAULT_PROJECT` when
configured. An unmapped repository without a default is rejected; issues that
use the default get a `repo:owner/name` label. `DISPATCH_NATS_DISABLED=1` leaves
database and live-socket paths available and makes `/healthz` report `nats: null`.
Dispatch loads config at every start, `DISPATCH_NATS_DISABLED` set or not, in this precedence
order: the user `envoy.json` (`${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json`), the `dispatch`
section of the checkout's `.envoy/envoy.json` in the directory it starts in, then environment
overrides. A `.envoy` that is a plain file counts as no checkout file. A checkout's
`.opencode/envoy.json` is not read: while the checkout has no `.envoy/envoy.json`, Dispatch logs
one WARN at start (`dispatch: a checkout's .opencode/envoy.json is not read`, with the rename as
`fix`). Each refusal below stops startup whether or not NATS is disabled, naming the file: a
checkout's file that holds an `envoy` section, naming `envoy` and the user file it belongs in (a
cloned repository does not choose the NATS servers the server connects to; the session clients
ignore such a section with a notice instead); a user file still at `~/.config/opencode/envoy.json`
with none at the current path, naming both and the move, while that path is
`~/.config/legion/envoy.json` (`XDG_CONFIG_HOME` unset, empty or `~/.config`), whatever
`DISPATCH_SERVER_URL` and `NATS_URLS` say; an `XDG_CONFIG_HOME` that is not an absolute path,
naming it; a file that carries a top-level `natsUrls`, naming `envoy.natsUrls` (a checkout's,
saying a checkout's envoy.json holds only a `dispatch` section); a JSON `null` at the top level, as
a section or as any key's value, naming the key; and JSON that does not parse, naming the line and
the column and never the text it stopped at, which can be the start of a token or seed. The server
reads `envoy.natsUrls` whatever `envoy.enabled` says: `enabled` switches a session's Envoy, not the
server's.
A present `NATS_URLS` overrides merged `envoy.natsUrls` before `bus.ConnectOwningStream` and the
outbox start; the server
owns `ENVOY_NOTIFICATIONS` on whichever server that resolves to, and refuses one that is not this
machine's unless `ENVOY_ALLOW_REMOTE_NATS=1` says the run means it (every deployment sets it). Every
NATS client of the server connects through `serverBus` (`bus.WithConnectInBackground`,
`bus.WithSilentCutDetection`): a NATS that is down or refuses the credential at boot refuses
nothing, the client connecting in the background and retrying forever, and only a configuration
error (an unusable seed or SOCKS5 setting, a server that is not this machine's) stops the boot; the
owning client reconciles the stream on its first connection rather than at boot. It creates and
reconciles the stream at the deployment's `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES`, two
settings table rows (`dispatchStreamOptions`), and the webhook redelivery bucket at the first; its
client's keeper raises the stream's copies and byte limit back to them, never lowering either, puts
`envoy_stream_id` back and raises that bucket every 30 seconds after (`bus.WithKeptBuckets`). The
stream holds the most copies and the largest limit any Envoy program of the deployment carries, so a
clustered deployment sets both alike here and in every envoy-listener; what a start and the keeper
do with them is `packages/envoy/AGENTS.md`'s "Operational notes". A
present `DISPATCH_SERVER_URL` overrides merged `dispatch.serverUrl`; it must be an absolute `http`
or `https` URL with no path, and, where the `DISPATCH_SIGNIN_*` settings
configure browser sign-in, is the exact browser origin used for it. It must
then equal the URL humans type into the browser, with
`<DISPATCH_SERVER_URL>/auth/callback` listed on the sign-in pool's app client. Host
adapters can override their configured Dispatch base URL with `DISPATCH_URL`.
`DISPATCH_TEST_HOOKS=1` mounts test-only routes — unset in every real deployment. `POST
/api/v1/live/_test/disconnect` closes every open `/ws/live` socket with 1012, as a server restart
does, so e2e can exercise the page's resume, which Dispatch replays in full (`replay_complete`
on its `events_start`). `POST
/api/v1/live/_test/rotate` closes every live socket with 4001 (`max age`), so e2e can exercise its
quiet resubscribe without waiting 30 minutes. `POST /api/v1/artifacts/_test/quiesce` closes every
live document, flushing each through the store, and waits for the settlements in flight, leaving
the service able to load documents again; `e2e/seed.ts` calls it before truncating so no document
the previous scenario had open writes after the truncate. `POST
/api/v1/artifacts/{id}/_test/outside-schema` writes the crafted malformed tree the document-repair
browser test uses. `POST /api/v1/agents/{id}/stream/_test/publish` (human only, as the agent view
is) feeds the in-memory agent relay the live-view browser rows run against: with no query it
publishes one frame, `?as=replay` sets what the next agent view is answered with, `?as=responder`
says whether anyone answers on its control subject, and `?as=drop` ends every open agent view as
`dropped`, so the page subscribes again and rebuilds from the replay set by then. `POST
/api/v1/writes/_test/slow` `{method, path, delay_ms}` (any signed-in caller; its body is strict, so
no `actor`) makes every keyed run of that method and exact request path wait `delay_ms` on its
handler's own context once its key is claimed, until a call with `delay_ms: 0`: the browser rows
drive a write Dispatch is slow to finish with it, slow on the server rather than held in the
browser. A context that ends during the wait answers `503 SLOW_WRITE_CUT_OFF` and the handler never
runs, so a keyed handler cut off with its client would never land under it.

The live-socket disconnect and rotate, the quiesce and the publish hooks act on every process
serving the database, not only the one the call reached, so a browser suite against two processes
reaches the page's process whichever one its request lands on: each process with the hooks on
listens on `dispatch_test_hooks` on a connection of its own, holding a session-level advisory lock
on it (namespace `HOOK`, keyed by that connection's backend pid); a call notifies every process,
each acts on itself and acknowledges on `dispatch_test_hook_acks`, and the call answers once every
process that held a lock when it was sent has acknowledged or lost its lock, with the first error
any reported, or `504 TEST_HOOK_TIMEOUT` after ten seconds naming the keys that did not answer
(`internal/dispatch/api/test_hooks_fanout.go`). `STREAM_UNAVAILABLE` is still judged on the
process the publish reached, and the outside-schema and slow-write hooks act where their call
lands. The flag costs one Postgres connection per process outside the shared pool
(`store.sharedPoolSize`).

With NATS configured and the GitHub App private key loaded, startup also runs the webhook
redelivery sweep (`internal/dispatch/redeliver`, wired in `cmd/dispatch/redeliver.go`). Every
`redeliver.Interval` it lists the App webhook's failed deliveries and redelivers them through
GitHub's App API. Its cursor, one record per delivery acted on and one index entry per open
delivery, keyed by the minute it next needs a sweep (`redeliver/index.go`), live in the
`envoy_webhook_redelivery` KV bucket, which the sweeper opens on its first pass that finds NATS
connected (`Sweeper.Open`), not at boot, since the server's NATS clients connect in the
background (`serverBus`). The running sweep reads from that index the deliveries due in its
window rather than every record in the bucket; a sweep given a window of its own, which is every
`redeliver-webhooks` run, reads every record. Its rate-limit
backoff lives in Postgres's `github_rate_limits` (scope `redelivery`, beside the App's `app` and
each installation's `installation:<id>`, which every process's `githubapp.Client` obeys;
`internal/dispatch/githubapp/ratelimit`), so the server hands the sweep its own store.
`envoy-dispatch redeliver-webhooks --since <d> [--dry-run]` runs the same sweep once over a
chosen window without moving the cursor; it opens the database `DATABASE_URL` names, as the
server does, and refuses to run without it, but migrates nothing. The README's "Webhook
redelivery" section has the rules: every failed delivery, 4xx included, is asked for again on a
backoff until GitHub records it `OK` or 72 hours after its first failed attempt, GitHub's rate
limits, the log lines. Like `natstail`, it connects as the nkey user `NATS_NKEY_SEED_FILE` (or
`NATS_NKEY_SEED`) names and runs no `!` command, so it never reads `envoy.natsSeed`: a run
against a NATS that requires a user needs `NATS_NKEY_SEED_FILE`.

With `DISPATCH_FILE_STORE_BUCKET` set, uploaded files' bytes live in that bucket under
`files/sha256/<hash>` (`internal/dispatch/files`; `main.go`'s `openFileStore` builds the store
and hands it down as `api.Deps.Files`), one object however often the file is uploaded; a
document's markdown stays in Postgres. `storeArtifact` writes the object before it opens its
transaction, so no issue lock or pooled connection waits on the bucket, and the row holds no
bytes; `getArtifactVersion` serves a row still holding bytes from the row and any other from the
bucket, streaming the body through `files.VerifyingReader` once the object has opened, and
aborting the response (`http.ErrAbortHandler`) on a body that fails or hashes wrong mid-stream.
Unset, `Deps.Files` is a nil interface and every upload stays in its row, as the test harness
and a laptop run. The API tests inject `filestest.NewMemory()`
(`internal/dispatch/files/filestest`); the store's own tests and `backfill-files`'s exit-code
test open the store with `files.NewS3` against `filestest.ServeS3`, a loopback bucket the AWS
SDK reaches through its own `AWS_*` environment, and once against an S3-compatible server in a
container (`internal/tests3`, SeaweedFS, which CI pre-pulls as it does the NATS image).
`envoy-dispatch backfill-files [--verify-only | --restore]` moves the rows uploaded before the
bucket, checks them, or writes them back (the rollback); the README's "Moving uploaded files to
the bucket" has its rules and the task role's grant. `/healthz` reports the bucket as `files`
and never fails on it. The bucket and its policies live in the deployment repository
(LEGION-520).

An artifact has one of three owners (migration `0071`, `artifacts_one_owner`): an issue, a
project, or an agent's conversation (`session_id` alone, `ref_key` `agent/<session id>/<slug>`,
addressed `dispatch://agent/<session id>/artifact/<slug>[@vN]`; `text.IsSessionID` is the one
rule a session id is held to, by the grammar and by `api/agent_artifacts.go`'s routes, which
answer anything else `400 INVALID_SESSION_ID`). `ref_key` is filled by the `artifacts_ref_key`
trigger, never by application code, and a lookup by owner compares `artifactOwnerKey`, the
trigger's own owner expression, except an agent's conversation's, which compares `session_id`,
that expression's session arm, so the partial index `artifacts_session_id` serves the two
lookups its upload makes (`agentArtifactByNameSQL`, `agentArtifactSlugTakenSQL`). An agent's
conversation holds files and images only: a markdown document uploaded to it is
`400 ARTIFACT_INPUT`, since a live document needs an event owner and every event belongs to an
issue, a document or a project. For the same reason its upload appends no event and its uploads
serialise on a transaction advisory lock keyed by the session rather than an owner row, and
`documentOwnerFromRequest` answers its file `400 ARTIFACT_AGENT_OWNED`, so no comment, ask, event
read or subscriber reaches one through `/api/v1/artifacts/{id}/...`. `api/agent_artifacts.go`
holds everything the artifact routes do differently for a conversation.
`serveFileVersion` serves every file and image version, whoever owns it, with
`Cache-Control: private, max-age=31536000, immutable`; a document version carries none.

Every owner's upload reads its kind from its bytes (`artifactKind`): a `text/markdown` upload is
a document, and any other is an `image` only when `http.DetectContentType` reads its bytes as a
PNG, JPEG, GIF or WebP, the pictures a model is shown, stored and served under that type whatever
the client declared. Anything else is a `file` under its declared type, so a client cannot store
arbitrary bytes, or an SVG, as an image. An artifact keeps its kind: a version whose kind is not
its artifact's is `400 ARTIFACT_KIND_MISMATCH`.

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
non-uuid `artifact_id`, since the view casts it on every row. Migration `0121_block_graph` makes
each block of a document a node: its tables (`document_blocks`, `block_refs`,
`document_block_versions`, `document_block_texts`, `document_indexed_versions`,
`block_grammar_owed`) and two more `graph_edges` arms, a block's mentions and its `part_of` edge
to its document. `0122`-`0125` add triggers that owe the block index what any build writes: a
document version carrying markdown, and a comment, ask or issue message whose text holds `#b-`;
`0126` owes it the comments, asks and issue messages already stored that hold `#b-`. `main` starts
`docs.Service.RunBlockIndexReconciliation` (lease `block-index`), which indexes what is owed with
no command run (`packages/envoy/AGENTS.md`, "References form one graph"). `0127` and `0128`
rebuild `asks_block_id_unique`, `asks_anchor_block_id_idx` and `comments_anchor_block_id_idx`
over `block_id_key(id)`, the id's SHA-256, so an ask block or an anchor under a block id of any
length is written. `0130` and `0131` add `comments.written_version` and `asks.written_version`,
the document version current when an unanchored item was written on a document (an approval
ask's, the version it names when it opens), with a `before insert` trigger on each table that
records it for every build's insert; `0132` creates `component_facts`, when each component was
made and last changed, which every import keeps; `0133` and `0134` fill `written_version` on the
rows stored before the triggers. The graph nodes' facts read them. `0142` makes each block of a
comment or message a node: `comment_blocks`, `message_blocks`, `comment_block_texts`,
`comment_block_state`, `message_block_state`, `comment_block_refs`, `message_block_refs`, the
nullable `replies_to_block` on `comments` and `messages`, and six more `graph_edges` arms under the
storage kinds `comment_block` and `message_block` (a block's `part_of` edge to its item, its
mentions, and a reply's `replies_to` edge to the block it names); `0143` and `0144` attach
`item_blocks_owe` to `comments` and `messages`, so every comment and message any build writes is
owed its blocks (`owed_at`) until a block-aware write or the block-index reconciliation indexes
it, and owe every item already stored.
`20261101144855_block_pointer_links` gives `block_refs` a nullable `to_types`, the types a typed
block's `block[]` pointer may point at, creates `block_ref_listed(to_types, to_id)`, and re-states
`graph_edges` with its `block_refs` arm listing a row only while that function holds, so a pointer's
edge, whose kind is the attribute's name, is listed only while its target has an allowed type
(`packages/envoy/AGENTS.md`, the typed block rules).
`envoy-dispatch rebuild-refs` reparses every document's latest version, ask, comment, and issue message,
reconciles `refs` with the text (surviving edges keep their provenance, orphan sources are
deleted), marks every document owed a block index and every comment and message owed a block
index of its own blocks, and prints counts, `blocks_owed` and `items_owed` among them;
it reads `DATABASE_URL` and the envoy config's `dispatch.server_url`, refusing an empty URL
because dashboard-URL mentions are recognised only against it.

`internal/dispatch/identity` is the human identity boundary. Handlers resolve
people through `Identity.Login`, which names a person by lowercase email, and
write identity errors with `identity.WriteError`.

- `DISPATCH_IDENTITY=cookie` is the default. Cookie identity signs people in
  through a Cognito sign-in pool or with Google directly (OpenID Connect
  authorization code, `internal/oidc/codeflow.go`) where the sign-in settings
  configure one: `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID`,
  `DISPATCH_SIGNIN_CLIENT_SECRET` and exactly one of `DISPATCH_SIGNIN_GROUP`
  (through the pool) or `DISPATCH_SIGNIN_DOMAIN` (with Google directly, under
  the issuer `https://accounts.google.com`). With none of the five set the
  server boots and runs no browser sign-in: nobody signs in, `/auth/start`
  answers `503 SIGNIN_UNCONFIGURED`, and the agent-token API is served, which
  is what a sandbox deployment runs. `checkSignInSettings`, reached only when
  at least one sign-in setting is set, is what refuses a partial one. It
  refuses, in this order,
  both set, some without the others (naming the missing ones), a domain under
  another issuer, a domain that is not a lowercase DNS name, and a group under
  Google's issuer; a deployment with the four Cognito settings reaches none of
  them. The authorization and token endpoints come from the issuer's discovery
  document, which `oidc.NewCodeFlow` does not read: `oidc.CodeFlow.Run`, started
  at boot, reads it in the background (retrying after one second, doubling to
  thirty), and so does the first call that needs it, so an unreachable issuer
  refuses no boot. Until it is read, `AuthURL`, `Exchange`, `Refresh` and
  `Revoke` fail with `oidc.ErrIssuerUnavailable`, which `/auth/start` and the
  callback wrap in `identity.ErrSignInUnavailable` and answer
  `503 SIGNIN_UNAVAILABLE`. The sign-in in flight rides the
  browser's `dispatch_oauth_state` cookie (`routes/oauthstate.go`: state,
  nonce, next and expiry, signed under a key `auth.SigningKeys.SignFor` derives
  from the current signing key), so any process completes a sign-in another
  started, and no process holds pending state. Those boot checks, the
  code flow and the request identity main builds live in
  `cmd/dispatch/signin.go`, and `identity.SignInRule` — an interface with two
  implementations, `identity.GroupRule` and `identity.DomainRule`, which
  `signInRule` chooses between once from boot's settings — carries the choice
  to the router and `identity.Membership`; each implementation's `Person`
  method reads only its own issuer's claims. Under a group it names the
  person by the email in the pool username (`<provider>_<email>`, the
  provider one the ID token's `identities` names), never by the `email`
  claim, which a person can write, and admits only a member of
  `DISPATCH_SIGNIN_GROUP` (`cognito:groups`); anyone else gets a 403 page
  naming them. Under a domain the code flow, built with the domain
  (`oidc.NewCodeFlow`'s hosted domain), asks Google for offline access with
  `prompt=consent` and `hd`, and reads `email`, `email_verified` and `hd`
  from the verified ID token itself (`readWorkspaceClaims`; `Verify`, shared
  with pods' tokens and the listener, reads none of them); `DomainRule`'s
  `Person` admits the lowercased `email` of a token whose
  `hd` is the domain and whose email is verified, and refuses anyone else with
  a reason on the 403 page (`ErrNotInDomain`, `ErrEmailNotVerified`,
  `ErrNoEmail`). The callback records the person in `people` with the issuer's
  refresh token, and under a domain the Google account's `sub` (`people.sub`,
  migration 0112): `PgPeopleStore.SignInSubject` ends every session of the address in
  the transaction that records a `sub` other than the stored one, so a failed
  revoke records nothing. `identity.Membership` renews that sign-in at least
  hourly, the age measured by the database (`now() - confirmed_at`): a refresh
  the issuer refuses (`oidc.ErrGrantRefused`, a `400` or `401` `invalid_grant`),
  or one whose ID token no longer passes
  the rule, names another email, or (under a domain) another `sub`, advances
  their session generation and forgets the token, ending every session they
  hold. Any other failed refresh (`oidc.ErrIssuerUnavailable`) ends nothing:
  `Login` returns it wrapped in `identity.ErrSignInUnavailable`, every caller
  answers that `503 SIGNIN_UNAVAILABLE` (`identity.WriteError`; a service
  token's own issuer outage stays `OIDC_ISSUER_UNAVAILABLE`), and the next
  request after the issuer answers
  refreshes. `store.PgPeopleStore` seals that token at rest
  (`store/refresh_token_seal.go`; the format is in the README's Identity
  section) under a key derived from the current signing key, and opens one
  sealed under any key of the set; main hands
  it the same `auth.SigningKeys` as the cookie identity (`openPeople`, `signin.go`). A stored value that does
  not open is no refresh token: `Membership` answers none and logs, writing
  nothing. Every boot retires each token stored in plain text
  (`retirePlainRefreshTokens`, `PgPeopleStore.RetirePlainRefreshTokens`):
  revoke at the pool (`oidc.CodeFlow.Revoke`), then compare-and-clear;
  `SignIn` retires the plain-text token it replaces through the same
  `retire`. A sealed value is never sent to the pool.
- `DISPATCH_IDENTITY=header:<Header-Name>` is for tests and local harnesses
  only, never for a production Dispatch deployment. It accepts the named
  header's value lowercased and records it in `people`; it requires
  `DISPATCH_IDENTITY_HEADER_TRUSTED=1` and refuses any `DISPATCH_SIGNIN_*`
  setting, so it cannot share a deployment with Google sign-in.
- `DISPATCH_ALLOWED_LOGINS` and `DISPATCH_APP_CLIENT_SECRET` are refused at
  boot as removed settings (`removedSettings`), naming what replaced them.
- `DISPATCH_DEV_SIGNIN=1` mounts `GET /auth/_dev/signin?login=<email>&next=<path>`
  (`routes/devsignin.go`, which holds everything that can mint a cookie without
  the sign-in pool), which records the lowercased person and issues the cookie
  identity's own session cookie through the callback's `issueSession`, with no
  pool exchange and no group check. `devSignInFence` (`cmd/dispatch/main.go`,
  run by `resolveBootConfig`) refuses it unless identity is cookie, every
  `DISPATCH_SIGNIN_*` setting is unset (a loopback server holds no sign-in
  client secret, and has no membership to confirm), the host of
  `boot.ListenAddr` (the one address the server serves on: the address `main`
  binds, or the address of the socket `DISPATCH_LISTEN_FD` names) is a loopback IP literal
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
  caller names, and can read GitHub through the App's proxy. The loopback check
  is on the host, not on what listens there, and every App call hands a signed
  App JWT to whatever owns that port, so the environment's key must be a
  throwaway, as `packages/dispatch/e2e/run-server.sh` generates one per run. The
  key is the credential that acts (`githubapp.New` builds no client without it,
  and the App JWT names the client ID), so the App ID is not fenced.
  `routes.BuildAppContext` also refuses a dashboard origin that is not
  loopback and stores the origin's host in the unexported `devSignInHost`, the
  only switch `New` reads, so no caller can mount the route without that check.
  The signing key is then
  `auth.NewSigningKey`, generated per process and never the data-dir file, so a
  cookie it mints dies with the process, unless `DISPATCH_DEV_SIGNIN_SHARED_KEY`
  is set (refused without the flag): every process given one value then signs
  with the base64url of HMAC-SHA256 keyed by it over the label
  `dispatch dev sign-in` (`devSignInSharedSigningKey`), never the value itself,
  so the browser suite's two-process mode keeps a person signed in across its
  two servers and a production key pasted there mints nothing production
  accepts. While it is on, the whole router
  answers a request whose `Host` is not the dashboard origin's
  `421 HOST_MISMATCH` (`requireHost`), and the route serves only a loopback peer
  with no forwarding header and logs every mint at WARN.
  The key bounds the cookie only: a `dsp_` token a dev session mints, and the
  session and token rows its sign-out changes, are rows every server on the
  same database acts on, so a dev-sign-in server needs a database of its own.
- The GitHub App (`DISPATCH_APP_CLIENT_ID` and `DISPATCH_APP_PEM_B64`, or the
  Dispatch app credentials file) signs nobody in; Dispatch uses it for webhooks,
  architecture sources and the web app's GitHub reads.
- Agents normally authenticate as a `session` actor with a personal `dsp_` token
  minted by a human in Settings, sent as `Authorization: Bearer <token>`.
  `DISPATCH_AGENT_TOKEN` is the shared devbox fallback; its callers have no
  owner attribution. It holds one value or several separated by whitespace, so
  the token can rotate with an overlap, and `resolveBootConfig` parses it once
  (`auth.ParseSharedAgentTokens`, which refuses an empty or repeated entry by
  position) into the one `*auth.SharedAgentTokens` that `appContextOptions`
  hands the router, so the settings tests drive the API from the variable. The
  API reads the bearer with `auth.BearerToken` and compares it with
  `auth.MatchesSharedAgentToken`, which compares it with every value in
  constant time and logs a match on a value after the first once per
  `X-Forwarded-For` address and User-Agent every 10 minutes
  (`cmd/dispatch/README.md` names the line's fields). Bearer callers cannot act
  as users. The document websocket (`/ws/doc/{room}`) takes no bearer at all: it
  is a person's cookie connection only (INFRA-264).
- `DISPATCH_OIDC_ISSUER` + `DISPATCH_OIDC_AUDIENCE` (both or neither; half the
  pair refuses to boot naming the missing one) make a Kubernetes pod's projected
  service-account token a third bearer credential. `resolveBootConfig` only
  reads the pair, through the shared `oidc.ConfigFromEnv` the listener also
  uses; the server's verifier is an `oidc.LazyVerifier` (`serviceTokenVerifier`),
  which reads the issuer's discovery document in the background from boot
  (`Run`) or at the first token, each attempt bounded at ten seconds, so an
  issuer that is unreachable — or one that accepts the connection and never
  answers — refuses no boot: until it is read a JWT-shaped bearer is answered
  `503 OIDC_ISSUER_UNAVAILABLE`, never a 401, and every other credential is
  served. A bearer is tried as the
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
- An access token the secrets broker issued is the fifth credential and the one
  that is not a bearer: it is presented under RFC 9449's DPoP scheme
  (`Authorization: DPoP <token>` with a `DPoP:` proof header), because that RFC
  refuses a DPoP-bound token presented under any other scheme.
  `api.optionalActor` therefore branches on the scheme before it reads any
  token, so a `DPoP` presentation is decided by `api.brokerTokenActor` alone and
  never falls through to a bearer branch, and the bearer branches above are
  untouched. `internal/brokertoken` does the deciding: the broker is asked what
  the token is (RFC 7662 introspection) and the caller is held to the `jkt` that
  answer carries. `brokerTokenVerifier` builds it from settings Dispatch already
  has and adds none: `DISPATCH_AGENT_SECRETS_URL` (the broker the approval
  relays already call), `DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE` (the
  projected ServiceAccount token every broker call presents, read at call time)
  and `DISPATCH_SERVER_URL` (the origin a proof's `htu` is checked against,
  never a forwarded header). Spent proofs go in Dispatch's own `dpop_proofs`
  (migration `0150`), swept beside the idempotency keys. A verified caller is
  `{kind: "session", id: <the token's session>}` carrying `owner`, the person it
  acts for, and `project`, the one project a named-scope token may write, both
  set from the token and never from a body as `service` is; a person-scoped
  token names no single project and carries none. Since the credential proves
  the session, `bearerSessionActor` takes a body with no `actor` at all, takes
  one naming that same session for its `origin`, and refuses one naming any
  other writer `403 ACTOR_SESSION_MISMATCH` naming the session the token
  carries. Its refusals are `401 BROKER_TOKEN_NOT_ACTIVE`,
  `401 BROKER_TOKEN_PROOF_INVALID`, `401 BROKER_TOKEN_KEY_MISMATCH`,
  `401 BROKER_TOKEN_NOT_A_SESSION` for a live token that is no session's
  credential — one naming no session or no person, or issued to a client other
  than the exchange, which a machine login's own access token is, so a body
  cannot name a session for a credential that proves none —
  `429 BROKER_TOKEN_RATE_LIMITED` and `503 BROKER_UNAVAILABLE`, which is a 503
  for the reason `OIDC_ISSUER_UNAVAILABLE` is: the broker answered nothing, so
  the token was never judged. A proof is single use by its `jti` and `htu`, so
  `api.decideBrokerToken` decides each request once for every layer that
  resolves its caller, and a websocket's heartbeat re-resolution asks the
  broker's answer alone (`brokertoken.Verifier.Live`) rather than presenting the
  upgrade's proof again. Without `DISPATCH_AGENT_SECRETS_URL` the branch has no
  verifier, and a `DPoP` presentation is the `401 UNAUTHORIZED`
  `invalid bearer token` that any Authorization header that is not a bearer has
  always been.

The GitHub proxy (`/api/github/rest/...`, `internal/dispatch/githubapi`) reads as the GitHub App
installation that covers the repository, for a signed-in person, GET only and only four paths: a
pull request (`repos/{o}/{r}/pulls/{n}`), an issue (`repos/{o}/{r}/issues/{n}`), a commit's
check runs (`repos/{o}/{r}/commits/{sha}/check-runs`) and a commit's workflow runs
(`repos/{o}/{r}/actions/runs?head_sha={sha}`, whose query is the full 40-hex `head_sha` and at
most a numeric `per_page` and `page`, each once), the only reads the web app makes: the checks pill
joins each check run to its workflow run for the workflow name and event, so it keeps only the
newest run of each check as GitHub's merge does, and drops the runs of an event GitHub does not
evaluate for a pull request (only `push`, `pull_request`, `pull_request_review`,
`pull_request_target`, `deployment` and `deployment_status` count). A workflow-runs answer of `200`
is trimmed to `total_count` and, per run in GitHub's order, only `check_suite_id`, `event` and
`name`, so a run's head commit (message, author), actor and repository never reach the browser; a
body that does not decode is `502 GITHUB_UPSTREAM`, and any other status but a rate limit passes
through unchanged.
GitHub's answer is read within 16 MiB rather than the proxy's usual 1 MiB, since each run carries
its repository and head commit (about 12.8 KB a run) and a page of 100 passes 1 MiB before it is
trimmed.
The pill needs the App's Actions: read, which the delivery reconcile already needs: without it
`actions/runs` answers `403` and the pill disappears. Every person shares the one App credential,
which can read far more of a repository than the web app shows, so the proxy forwards nothing else:
another method is `405 METHOD_NOT_ALLOWED`; another path, a workflow-runs read with any other
query, or a path under it (`actions/runs/{id}`, its jobs or logs) `404 GITHUB_PATH_REFUSED`; and a
repository the App is not installed on, or a deployment with no App key,
`503 GITHUB_TOKEN_UNAVAILABLE`. A GitHub rate limit, answered to the token mint or the read, or
recorded by any process in `github_rate_limits` (then GitHub is not asked), is
`503 GITHUB_RATE_LIMITED` with `Retry-After`.

## Routes

The `/api/v1` routes are one table, `api/routes_table.go` (`routes()`): `Register` mounts it and
`GET /api/v1` (public) serves it as `{routes: [{method, path, auth, description}], docs}` sorted by
path then method. Add a route by adding a row — never a `mux.HandleFunc` line — and bump the pinned
count in `api/routes_table_test.go`. `auth` is `public`, `any` (user or bearer), `human`, or
`bearer`; it describes the check the handler makes, the handler still enforces it. An unknown path
under a server root (`/api`, `/v1`, `/auth`, `/ws`, `/healthz`) is answered by
`routes/router.go` with `404 {"code":"NOT_FOUND","error":"no route for <METHOD> <path>","hint":"GET
/api/v1 lists every route"}` before any dashboard lookup; only paths outside those roots fall back
to the SPA shell. Every page the static handler serves (`index.html` and that fallback) carries
`Cache-Control: no-cache` and an `ETag` of its bytes, never a `Last-Modified` (`servePage` in
`routes/router.go`): a browser revalidates it before running it, gets a 304 only for the page this
server holds, and gets this server's page in place of any other, since the servers behind one load
balancer can hold different builds whose pages' file times say nothing about which is newer (a
rollback serves the older file). A file under `/assets`, Vite's content-hashed output, carries
`public, max-age=31536000, immutable`. Every server process publishes its web dist's assets into
Postgres at boot, before it listens (`publishDashboard` in `main.go`, `internal/dispatch/dashboard`,
migration `0136`): one transaction under a shared advisory lock stores each file not yet stored
under its key (`assets/<relative path>`), the build's row and its links, so a restart on a stored
build writes no bytes. A publish that fails, a file over `routes.MaxRetainedAssetSize` (8 MiB)
among them, exits the boot before the process is ready; a dist with no `index.html` (the Go test
rigs) publishes nothing and serves no dashboard. The prune that follows, under the same lock held
exclusive, keeps every build a live server process serves (`processes.Live`) and the
`dashboard.KeepNewest` (20) started most recently, then deletes every asset no build links; a failed
prune is logged and the boot goes on. A local miss below an asset root (`isRetainedAssetPath`) — a
chunk of another build, asked for by a page another process served while a rolling update or a
rollback runs two builds — is answered from the database (`dashboard.Store`, the router's
`AssetStore`). `serveRetainedAsset` first looks in the process's `assetCache` (`routes/assets.go`),
which keeps up to `maxCachedAssetBytes` (64 MiB) of what the store answered, least recently used
evicted first, an object larger than the whole cache not kept; a cached object is served at once.
On a miss, concurrent requests for one key share one store read (singleflight), bounded by three
seconds from its start and not by the first request's context, so that client going away fails
none of the others; a failed read caches nothing. Only once the whole object is read does it set
the immutable header and write it through `http.ServeContent`, so a failed read is a 502 with no
cache header, and the browser's download is not held to the three seconds. A path no build
published is a 404 without caching. Every 502 logs `dispatch: retained asset store failed` at
ERROR with the key and the error. An object stays in memory until its client has read it while the
server sets no write timeout, so the router's `retainedHeld` semaphore caps the bytes misses hold
at once at `maxRetainedBytesHeld` (64 MiB). A miss reserves 8 MiB before it asks the store, waiting
within its three seconds for earlier requests to give theirs back, so a stale tab's burst of chunks
is served in turn; it keeps only its object's size once it is read, and gives that back when its
response is written, its client goes away or the read fails. A request that cannot reserve room, or
whose admitted fetch then runs out of that same three seconds before the store answers (most often
because most of the bound was already spent waiting for room), is a 503 with `Cache-Control:
no-store` and `Retry-After: 1`, logs `dispatch: retained asset refused at the memory cap` at WARN
with its key, and — for the room refusal — asks the store nothing: the shared bound expiring is
never presented as a store failure, since the store was only asked too late to answer in time. A
request whose own client disconnects while still queued for room or while the store reads is a
different case: the cap was never the reason, there is nobody left to answer, and the handler logs
nothing above a debug line and writes no response at all. It never asks the store for a page or
another path. A router with no `AssetStore` leaves every local asset miss a 404. Any other file
(the favicon) carries no cache header.

Every `/api/v1` route accepts an authenticated user or an agent bearer unless
the table says human only.

Every `POST` and `PATCH` route takes an optional `Idempotency-Key` request header (1 to 128
letters, digits, `.`, `_`, `:` and `-`, bare or double-quoted; anything else is
`400 KEY_INVALID`), and none requires it: `register` wraps each such route in
`keyedWrites` (`api/request_keys.go`; `packages/envoy/AGENTS.md`, "Keyed Dispatch writes"), so a
repeat with the same key and request once the first has finished is answered the first's stored
status, headers and body with `Idempotency-Replayed: ?1` and runs nothing; the same key with a
different request is `422 KEY_REUSED`; a repeat while the first still runs, whose handler finishes
on the server whatever its client does (within the 30 s stop grace on a shutdown, past which the
lease handover takes its key over) and renews its 60 s lease every 20 s until its ten-minute
limit, is `409 KEY_IN_FLIGHT` with `Retry-After: 1`. Such a repeat is never spooled: the key layer
reads its body off the connection and discards it, hashing it on the way, within `bodyDrainLimit`
(a minute), then reads the key again and judges it by that digest (`answerDrainedRepeat`), so a
different body is `KEY_REUSED` and a first that answered meanwhile is replayed; a
5xx, 401, 408 or 429 is not stored, so the next
attempt runs again; and a repeat whose first request committed a write but whose answer was lost
(its lease lapsed, or it ended in a 5xx or a panic after committing) is `409 KEY_COMMITTED` with no
`Retry-After`, never run again, which a client treats as a landing. The issue message, both
comment routes, the single-session send and the broadcast commit a recovery point with what they
made, so their repeat answers `201` from a fresh read instead and resumes what delivery it owes,
which the delivery sweep also finishes on its own. Keys belong to the caller's login and are kept
24 hours (`request_keys`, migration `0137`, swept by the maintenance sweep's pass once a minute).
A write without the header runs unkeyed, as before. `PUT` and `DELETE` take no key. Every write
route, `PUT` and `DELETE` included, that names its presser in `Dispatch-Login` runs only as that
person: another person, or any bearer, is `409 LOGIN_CHANGED` before anything runs, its key
untouched; a caller that resolves to nobody keeps the route's own `401` (`refuseChangedLogin`,
`api/server.go`).

| Path | Method | Access | Purpose |
| --- | --- | --- | --- |
| `/api/v1` | GET | public | List every `/api/v1` route with method, auth, and purpose. |
| `/auth/start` | GET | public | Start Google sign-in at the sign-in issuer (the Cognito pool, or Google for `DISPATCH_SIGNIN_DOMAIN`, with offline access, `prompt=consent` and `hd`), the sign-in's state in the signed `dispatch_oauth_state` cookie; `503 SIGNIN_UNCONFIGURED` on a server with no `DISPATCH_SIGNIN_*`, `503 SIGNIN_UNAVAILABLE` and no cookie while the issuer is not discovered. |
| `/auth/callback` | GET | sign-in state | Verify the `dispatch_oauth_state` cookie under the key set (any process completes a sign-in another started; `400` "invalid or expired state" for one missing, tampered, lapsed or naming another state), exchange the issuer's code and sign in the person `SignInRule.Person` admits (`GroupRule` or `DomainRule`): a member of `DISPATCH_SIGNIN_GROUP` named by the email in their pool username (a 403 page naming anyone else), or a verified Google account of `DISPATCH_SIGNIN_DOMAIN` named by its `email` (a 403 page with the reason for anyone else); record them in `people` with the refresh token sealed (and, under a domain, its `sub`, ending the address's sessions when another account held it), and issue the session cookie. `503 SIGNIN_UNAVAILABLE` while this process has not discovered the issuer; `409 SIGNIN_SUPERSEDED` and no cookie when, under a domain, another account signed in to the address between this sign-in's record and its cookie (`issueSession`). |
| `/auth/logout` | POST | identity | Advance the person's session generation, forget their refresh token, and clear the cookie. Local only: the issuer's own session is left alone. |
| `/auth/whoami` | GET | identity | Return the resolved person, `{kind: "user", login, event_head}` with `login` their lowercase email and `event_head` an event id at or below which every event is final, read when the sign-in check answered (`api.Background`'s `EventHead`). The page uses that head as its first events subscription's resume point, so reads begun after sign-in cannot miss an event. The sign-in check comes first: a caller who is not signed in is `401 NO_IDENTITY` and the head is never read. A signed-in caller is `503 EVENT_HEAD_UNAVAILABLE` while no event id is proven final yet (`api.ErrEventHeadUnproven`), which the page asks again for as long as it lasts, and `503 EVENT_HEAD_READ_FAILED` for any other failure to read the head, which the page retries on its bounded rule. |
| `/auth/_dev/signin` | GET | public, `DISPATCH_DEV_SIGNIN=1` only; loopback peer, no forwarding header | Record the lowercased person `login` names and issue their session cookie with no issuer exchange, then redirect to the sanitized `next`; `400 DEV_SIGNIN_INPUT`, `403 DEV_SIGNIN_FORBIDDEN`. Not mounted otherwise. |
| `/api/github/rest/...` | GET | identity | Read a pull request, an issue, a commit's check runs or a commit's workflow runs from GitHub as the GitHub App (above). |
| `/healthz` | GET | public | Readiness, not liveness: `200` while Postgres answers within two seconds on the health pool, `503` while it does not (`ok` and `db` say the same). Also reports, never deciding the status, `nats` (the publisher connected to NATS, `null` with NATS disabled), `files` (the bucket answers a two-second `HeadBucket`, `null` with none configured), `signin` and `service_tokens` (each issuer discovered, `null` when not configured), plus `commit` (this process's build's legion commit, or `null`), `fleet_commit` (the commit of the latest announcement that every live server process serves one build, or `null`) and `schema_version` (the highest applied migration, or `null` when the database did not answer). |
| `/api/v1/projects` | GET, POST | user or bearer | List projects (including `open_asks`) or create one from `key` and `name`, whoever asks: a person or an agent session, which names itself in `actor`. A key that is not a project key, an empty name, a name holding a control character (Unicode Cc) or a bidi embedding, override or isolate (U+202A-U+202E, U+2066-U+2069), or a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF); combining marks, format characters, private-use characters and whitespace don't count) is `400 INVALID_PROJECT`; every other format character (an emoji's joiner, a flag's tags, a direction mark, a soft hyphen) and combining mark is allowed beside a visible one; a name over `contracts.ProjectNameMax` (80) UTF-16 units after the trim is `400 CAP_EXCEEDED`; a key that already exists is `409 PROJECT_EXISTS`. |
| `/api/v1/projects/{key}/artifacts` | GET, POST | user or bearer | List non-primary artifacts in a project (`?unlinked=true` selects unlinked ones) or create an unlinked project artifact, its kind read from its bytes as an issue upload's is. An ask block whose body breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes. A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose markdown makes more than 65,536 elements (weighed as `README.md`'s Element weights lists), naming the line that passes the limit; a document or version whose markdown as stored (what `.../text` answers) would be longer than 1 MiB and longer than the document's was, or make more than 65,536 elements and more than the document's did, or that would leave a live state ygo could not decode as one update; or whose formatting is more items than one document update can store (1,048,576), naming the count. A file the configured bucket refuses is `502 FILE_STORE_UNAVAILABLE`, with no version written. |
| `/api/v1/settings/repo-projects` | GET | user or bearer | List repository-to-project mappings, each with `default`: whether it is its project's default repository. |
| `/api/v1/settings/repo-projects/{owner}/{repo}` | PUT, DELETE | user or bearer | Create or replace, or remove, a repository mapping. PUT takes `{project, default?}`: `default: true` makes the mapping its project's one default and clears the previous default in the same write; `default` absent keeps the mapping's default while its project is unchanged, and a mapping moved to another project is not that project's default unless the body says so. A mapping an issue names as its `repo` is neither deleted nor moved to another project: `409 REPO_PROJECT_IN_USE`, naming how many issues name it. Every mapping a write changes emits `settings.repo_project.updated` `{mapping, deleted}` on its project, and a moved mapping one on the project it left too, `deleted: true` there. |
| `/api/v1/github/app-installation/{owner}/{repo}` | GET | user or bearer | Whether Dispatch's GitHub App, which GitHub sends the repository's webhook events through, is installed on the repository: `200 {installed}`, `false` when GitHub answers 404 (no permission is checked). `503 GITHUB_APP_UNCONFIGURED` when Dispatch holds no App key, `502 GITHUB_UNREADABLE` naming the repository and GitHub's error on any other failure. The Legion daemon asks it for every repository it serves and names each one without the App in `legion state`. |
| `/api/v1/settings/architecture-sources` | GET | user or bearer | List every project's architecture sources with their sync bookkeeping, by project and then in the order each project added them. |
| `/api/v1/settings/project-default-assignees` | GET | human only | List every project that names a default assignee, by project key, as `[{project, assignee}]`; a project with no default is left out and no default anywhere is an empty list. The default is the last step of a create's assignee chain, and human-only because it names a person. |
| `/api/v1/projects/{key}/architecture-sources` | GET | user or bearer | A project's architecture sources, one per repository, in the order they were added; each row is `{project, repo, branch, enabled, created_by, created_at, last_sync_at, last_commit, last_error, components}`, `components` being how many components that source projects now. The project's component model is every source's components together. A project with no source answers `200 []`, since whether a project has a model is a question with an ordinary negative answer; an unknown project is `404 NOT_FOUND`. |
| `/api/v1/projects/{key}/architecture-source` | GET | user or bearer | Kept for `dispatch` clients older than this release, which read it on every issue create; the list route above replaces it, and each call logs a warn naming the project. `200 null` when the project has no source, otherwise `200` with its first source's row (in id order; those clients read only whether it is null); an unknown project is `404 NOT_FOUND`. The `dispatch` CLI of this release reads it only when the list route has no route (an older server). |
| `/api/v1/projects/{key}/architecture-sources/{owner}/{repo}` | PUT, DELETE | user or bearer | One source, addressed by its repository (stored lowercase with `.git` trimmed, as repository mappings are; the directory is fixed at `.dispatch/architecture/`). PUT takes `{branch, actor?}` (a bearer names its session in `actor`), proves the GitHub App can read the repository and that `branch` is the name of a branch it has first (one exact `git/ref/heads/{branch}` read, `githubapp.Client.Branch`): a missing branch, a commit SHA, a relative ref such as `main~1`, `HEAD` or a `refs/` path is `409 SOURCE_ACCESS` naming the value and the repository, as a credential problem is, writing nothing. It then adds the source or changes the branch of the one it already is, keeping its components and resetting its sync bookkeeping; `200` answers the row. DELETE removes the source and the components it projected (no other source's), `204`, or `404 SOURCE_NOT_FOUND` when the repository is not a source of the project. Both emit `settings.architecture_source.updated` `{source, deleted}`. |
| `/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync` | POST | user or bearer | Import that source's model now; `200` carries its updated row (`last_error` set when the model was rejected and the source's previous projection stays up), `409 SOURCE_ACCESS` a credential or branch problem, `404 SOURCE_NOT_FOUND` when the repository is not a source of the project. |
| `/api/v1/projects/{key}/architecture-sources/sync` | POST | user or bearer | Import every source of the project now, each attempted whatever another's import did; `200` carries a JSON array of every updated row in the order the sources were added, each with its own `last_error` (a credential or branch problem included: it is on that row, not a 409). `404 SOURCE_NOT_FOUND` when the project has no source; `500` only when a failure could not be recorded on its row. |
| `/api/v1/projects/{key}/architecture-source/sync` | POST | user or bearer | Kept for `dispatch` clients older than this release, whose `dispatch architecture-sync` calls it; the every-source sync above replaces it, and each call logs a warn naming the route and the project. Imports every source as the every-source sync does and answers one row: the first source in id order whose `last_error` is set, or the first source when none failed. `404 SOURCE_NOT_FOUND` when the project has no source; `500` only when a failure could not be recorded on its row; a credential or branch problem is recorded on its row and answers `200` with that `last_error` (those clients print `Sync failed for …`), not the `409 SOURCE_ACCESS` it once was. |
| `/api/v1/projects/{key}/architecture` | GET | user or bearer | The component tree with the work attached: `sources` (`[{repo, branch, last_commit, last_sync_at, last_error}]` in the order the sources were added), `totals` (`issues_done`/`issues_total` over every filed issue, `unassigned`, `not_architectural`, `components_without_work` over non-external components, `retired_links`), one row per component of every source (`source` is the repository that defines it; `done`/`total` count the distinct issues whose effective set names it or any component it contains, parents and icebox included, each once; `own_*` only those naming it itself; `issues` say how each qualified: `direct` > `inherited` > `contained` with `via`), and the `unassigned`, `not_architectural` (with `reason`, `inherited_from`), and `retired_links` lists. `404 SOURCE_NOT_FOUND` without a source. |
| `/api/v1/projects/{key}/default-assignee` | PUT | human only | Set a project's default assignee `{assignee}` — the email of a person who has signed in, or `null` to clear it — who an issue created with no assignee, by nobody, under no parent or under a parent with nobody on it, goes to. An email nobody has signed in with is `400 ASSIGNEE_NOT_ALLOWED`, a body without `assignee` (or one whose `assignee` is neither a string nor null) `400 INVALID_PROJECT`, an unknown project `404 NOT_FOUND`; the write appends `settings.project_default_assignee.updated` and answers the stored `{project, assignee}`. Human-only: no agent command, flag or route sets a project's default. |
| `/api/v1/me/agent-tokens` | GET, POST | human only | List personal token metadata or mint a personal agent token. |
| `/api/v1/me/agent-tokens/{id}` | DELETE | human only | Revoke a personal agent token. |
| `/api/v1/users` | GET | human only | Everyone who has signed in (`people`), as `{users: [{login}]}` sorted by email: the assignee picker's options. |
| `/api/v1/whoami` | GET | user, bearer, or a broker-issued DPoP token | Who the server takes the caller for: `{kind: "user", login}` for a person (`login` their lowercase email), `{kind: "agent", owner, service, session, project}` for every other credential (`owner` is the personal token's owner or a broker-issued token's person by lowercase email, null under the shared token; `service` is a verified service-account token's Kubernetes subject, null for every other credential; `session` is the session the credential itself proved, which only a broker-issued token does, null otherwise; `project` is the one project key that credential may write, null where it names none). |
| `/api/v1/dashboard/builds` | GET | user or bearer | Which dashboard builds are served (`api/dashboard_builds.go`): `{serving, live}`, `serving` the entry chunk (`/assets/index-*.js`) of the build this process serves (`""` when it serves none), `live` the distinct entry chunks of the builds the live server processes serve (`processes.Live` joined to `dashboard_builds` on the build id), sorted. The dashboard's "A new version is available" notice offers itself only when `live` holds exactly one entry and it is not the tab's own, so no tab is told to reload while a rolling update or a rollback still runs two builds. |
| `/api/v1/issues` | GET, POST | POST human or bearer | List or create native issues. The listing is every matching issue as an array, or, with `limit` (1–250) or `offset` (0 or more; alone it pages 50), one page `{issues, total, limit, offset}` cut after every filter, `total` counting the issues they match; a repeated, blank, non-integer or out-of-range value, or `cursor`, is `400 INVALID_QUERY` naming the parameter. Each listing row carries `blocked_by`, the sorted keys the issue waits on, `primary_artifact_id`, and `repo` and `resolved_repo` as the issue read below describes them. Creation without a `spec`, or with a blank one, gives an empty primary document at version 1. Creation takes the issue's own `repo` (`owner/name`, canonicalised as a mapping's is): one that is not `owner/name` is `400 INVALID_ISSUE`, and one not mapped to the issue's project `400 REPO_NOT_MAPPED`. A `parent` that would put the new issue more than 32 links (`parentDepthCap`) below its root is `409 PARENT_INPUT`. A title over `contracts.IssueTitleMax` (1,000) UTF-16 units, counted after trimming, is `400 CAP_EXCEEDED` before the duplicate check reads the project's titles. Creation refuses a title that near-duplicates an issue in the project with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true; external references skip the check. A spec whose ask block breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`. A spec whose markdown makes more than 65,536 elements, or whose markdown as stored (what `.../text` answers) would be longer than 1 MiB or make more than 65,536 elements, is `413 CAP_EXCEEDED`. `blocked_by` names at most `contracts.MaxIssueBlockers` same-project issue keys before deduplication: an over-limit list or unknown key is `400 BLOCKED_BY_INPUT`, another project's issue `400 BLOCKED_BY_OUTSIDE_PROJECT`, and a wait cycle through `blocked_by` and parent links `409 DEPENDENCY_CYCLE`. Non-empty blocker lists and a `parent` take a per-project dependency advisory lock before allocating the number or rank; creation with neither takes no dependency lock, and no creation takes an explicit parent-row lock. |
| `/api/v1/search?q=&project=&limit=&offset=` | GET | user or bearer | Full-text search over issue titles, latest document text, comments, asks, and messages; each kind is ranked on its own list and the lists are merged by reciprocal rank fusion (`README.md`, "Search"), with `<mark>` snippets and SPA `href`s. One page `{results, total, reachable, limit, offset, took_ms}`: `limit` is 1–50 (default 20) and `offset` 0 or more (default 0); `total` counts every match and `reachable` the hits the pages can return, each kind's best `contracts.SearchKindDepth` (100). An under-two-character query returns `400 INVALID_QUERY`, while a stop-word-only query returns `200` with no results; a query over `contracts.SearchQueryMax` (1,000) UTF-16 units, counted after trimming, returns `400 CAP_EXCEEDED` before anything runs, a project that is not a project key returns `400 INVALID_PROJECT` (an empty one searches every project), a repeated, blank, non-integer, or out-of-range limit returns `400 INVALID_LIMIT`, and the same for offset returns `400 INVALID_OFFSET`. They ride in the URL; `packages/contracts/AGENTS.md` "Search limits" owns what keeps it under the load balancer's limit. Keyword search finds a text whose search vector would pass Postgres's limit on one vector by the words in its opening part only (`README.md` "Search"). |
| `/api/v1/search/coverage` | GET | user or bearer | Meaning search's coverage (`api/search_coverage.go`, `embedqueue.Coverage`, one statement over `coverageSources`): `{identity, place, model, total, kinds}`, the embedder search embeds its queries with (all three null with none), then in total and per kind `items`, `current`, `lacking`, `failed` and `oldest_lacking_seconds`, as `model.SearchCoverageCounts` defines them. |
| `/api/v1/projects/{key}/related?title=&text=&exclude_issue=&keywords_only=` | GET, POST | user or bearer | The related-item check an issue create runs (`api/related_routes.go`; `relatedCheck` with `record` false), on a title and a text, writing nothing. GET's title and text together are capped at `contracts.SearchQueryMax`; POST's JSON body `{title, text, exclude_issue, keywords_only}` caps the title at `contracts.IssueTitleMax` and the text at the body's bound. `exclude_issue` must be an issue of the project (`400 EXCLUDE_ISSUE_INPUT`) and is left out of the results and the term statistics. Answers `model.Suggestions` within 5 s; a blank text is `200` `not_made`. |
| `/api/v1/related-checks?project=&source_kind=&from=&to=` | GET | user or bearer | Counts and timing percentiles over `related_checks` within `[from, to)` (`api/related_routes.go`, `model.RelatedChecksReport`); a `source_kind` other than `issue`, `ask` or `decision_block` is `400 INVALID_QUERY`. |
| `/api/v1/issues/{key}` | GET, PATCH | PATCH human or bearer | Read or update an issue. A `title` is trimmed, and one over `contracts.IssueTitleMax` (1,000) UTF-16 units is `400 CAP_EXCEEDED`, as on creation. `assignee` (the email of a person who has signed in, lowercased; `null` clears; absent leaves it) may be set by any caller; an email nobody has signed in with is `400 ASSIGNEE_NOT_ALLOWED`. `components` is the issue's own architecture attachment: `null` or `{mode: "inherit"}` deletes it (the issue takes its nearest ancestor's again), `{mode: "explicit", ids}` names bare component ids of the issue's project (`400 COMPONENTS_INPUT` for an unknown, retired, external, or other-project id), `{mode: "none", reason}` declares the issue not architectural; it is one of two fields besides `rank` a closed issue accepts without reopening, the other being an `external_links` write that adds no URL the issue does not already have (so a link can move to the issue that replaced it). Every issue read carries the effective `components` (`mode`, `ids`, `unknown` for retired ids, `reason`, `inherited_from`), resolved up the parent chain in the same query. `repo` is the issue's own repository, `owner/name`, which must be mapped to its project (`400 INVALID_ISSUE` when it is not `owner/name`, `400 REPO_NOT_MAPPED` when it is not mapped; `null` clears it; absent leaves it; a closed issue refuses it, `409 ISSUE_CLOSED`). Every issue read and `issue.*` payload carries `repo` and `resolved_repo`, the repository the issue targets: its own `repo`, else its nearest ancestor's, else its project's default mapping, else `null`, resolved in the same query. Every issue read and `issue.*` payload carries `blocked_by`, sorted. A PATCH `blocked_by` replaces the complete list (`[]` clears it) with the creation count limit and refusals, and naming the issue itself is `400 BLOCKED_BY_INPUT`; a parent change that would leave an issue waiting on itself through `blocked_by` and parent links is `409 DEPENDENCY_CYCLE`, and one that would put the deepest issue under it more than 32 links (`parentDepthCap`) below its root is `409 PARENT_INPUT`. Blocker-list and parent writes serialize on a per-project advisory lock acquired before row or rank locks, and read the graph without locking its rows. Every PATCH then locks every issue row it names together in one key order: the issue, its current parent when a `status` or `parent` write appends to it, the proposed parent and the `rank` neighbours; a parent a concurrent reparent moves at both attempts is `409 PARENT_CONTENDED`, with nothing applied. Other PATCH fields take no dependency lock or new refusal. A close (`status: done`) first settles the issue's documents that owe a settlement, then, in its own transaction, withdraws every open ask of the issue, whoever's turn it is - decision blocks, plain asks and approval requests (`withdrawIssueAsksTx`): each is recorded `resolved` with resolution `{kind: "withdrawn", actor: <the closer>, reason: "Withdrawn by the close of <KEY>.", at}` and gets one `ask.resolved` before `issue.closed`, and each withdrawn block, and each copy of it in another document of the issue, is written `state="resolved"` in the live document with no version; an ask block still without an ask row is written `state="resolved"` with no record, logged. An ask answered or resolved before the close is left as it was, and the close records no answer, review or approval. A reopening `status` opens again exactly the asks the close withdrew (kind `withdrawn`), with `state: open`, no answer and no resolution and everything else kept, each with one `ask.opened` before `issue.updated`, and their blocks read `state="open"` again. |
| `/api/v1/issues/resolve` | GET | user or bearer | Resolve an external issue reference to its native key. |
| `/api/v1/issues/{key}/summary` | GET | user or bearer | What a reference to an issue shows, read without the full issue (`api/issue_summary.go`): `{key, project, title, status, documents}`, each document `{id, slug, name, kind, latest_version, primary}` in the order the issue read lists its artifacts (`primary` marks the spec, which a reference to a block of the spec names); `404 NOT_FOUND` for no such issue. |
| `/api/v1/issues/{key}/events` | GET | user or bearer | Read events by forward cursor, descending page, or exact IDs. |
| `/api/v1/issues/{key}/references?versions=` | GET | user or bearer | Read the eight-hop artifact reference closure; a reference to a block reaches the block's document while a current version holds the block (`refs.Closure`). Matching `If-None-Match` returns `304`. `versions=latest` leaves each member's `artifact.versions` holding only its latest version and carries its own ETag; absent, empty or `all` is the full answer; any other value, or `versions` given more than once, is `400 INVALID_QUERY`. |
| `/api/v1/references?to=\|from=&kind=&since=` | GET | user or bearer | Edges of one node in the reference graph, newest first, cross-project. Exactly one of `to` (backlinks) or `from` (links), each a `dispatch://` reference read whatever its length (`text.ParseRef`; the text bounds decide only what the graph indexes); `kind` is a csv of the structural kinds `model.GraphEdgeKinds` (`mentions`, `child_of`, `blocked_by`, `attached_to`, `anchored_to`, `owned_by`, `replies_to`, `followed_by`, `part_of`, `depends_on`, `affects`) and the `block[]` attribute names the block schema declares (`pmdoc.LinkKinds`), any other `INVALID_KIND`; `since=<events.id>` keeps mentions introduced after it and excludes structural edges. The answer is `{node, edges}` (`model.GraphReferences`). Each edge carries the other `node` (`kind`, `id`, `issue_key`, `project`, `ref`; no `ref` for sessions; a `component` node is `<project>/<id>` with ref `dispatch://<PROJECT>/component/<id>`, and one a re-import retired is omitted with its edges; a `block` node is `<document ref_key>#<block id>` with `block_id`, `document` and `changed_version`, or, for a block of a comment or message, `comment/<uuid>#<block id>` or `message/<uuid>#<block id>` with `block_id`, no `document`, its `parent` the item and a comment block's `changed_version` the comment revision its text last changed in, and one no current version or revision holds is omitted with its edges), an `excerpt` (the containing block, with `block_id`, for a document mention or a block's; the node's text head otherwise), `created_at`, and `source_seq`. Every node, the one read, each edge's `node` and each edge's `block`, also carries `made_by`, `made_at`, `changed_by`, `changed_at`, `parent` (`model.NodeParent`: `kind`, `id`, and `ref` unless the parent has none) and `parent_version`, always present: an actor list Dispatch never recorded is `[]`, any other fact `null` (`packages/envoy/AGENTS.md`, "References form one graph", gives each kind's sources). A typed block's `block[]` pointer is an edge of the attribute's kind from the typed block, listed only while `block_ref_listed` holds (its target has a type the attribute allows). A document's, comment's or message's mention of a target one of its blocks holds a link of any kind to is listed from that block alone (`heldByABlock`); a document's, comment's or message's own read also lists the edges at its current blocks, each naming the block as `block`, and leaves out `part_of` unless `kind` names it, and lists a reply naming one of its current blocks once, under that block. A block reference (`#b-<id>`, the id as `encodeBlockId` writes it) names a node carrying `text`, the block's whole current text; pinned (`#b-<id>@v<N>`, a document's version or a comment's revision) it also carries `pin` (`model.BlockPin`: `version`, `ref`, `text` at that version, `state` `unchanged`, `changed` or `removed`, `removed_version`), and a removed block answers `edges: []` (`blockReferenceNode`, `itemBlockReferenceNode`), its node a removed block's (`refs.RemovedBlockNode`, `refs.RemovedCommentBlockNode`). `400 INVALID_REFERENCE` / `INVALID_KIND` / `INVALID_SINCE`, `INVALID_REFERENCE` for a message block pinned with `@v<N>` too; `404` when the node does not exist, for an unpinned block no current version or revision holds or whose id is past `text.MaxBlockIDBytes` (no node), and for a pin to a version the document lacks, has no record of the blocks of, or that did not hold the block, or to a revision past the comment's current one or that did not hold the block. A `since` read stops at the event log's safe horizon (`events.Horizon`), and first waits up to 1 s (`referencesSinceWait`) for the horizon to pass every event committed before the read, so it answers every mention committed before it, the caller's own included; when an open transaction holds the horizon back past that wait, it answers the mentions at or below the horizon and a later read the rest. |
| `/api/v1/block-links?from=&to=` | GET | user or bearer | Links from blocks of the document `from` names to blocks of the document `to` names (an issue's spec or document, or a project's document; the two may be one), each read by `text.ParseRef` whatever its length, `refs.BlockLinks`: `{from, to, links}` (`model.BlockLinks`), each link `{kind, from, to, created_at, created_version}` with both block nodes, newest first, then by the source block's node id, then by the target's; every node carries the six facts the `/api/v1/references` row names. A link is listed only while both blocks are current, and a `block[]` pointer's only while `block_ref_listed` holds. `400 INVALID_REFERENCE` for a parameter that is not a document reference; `404 NOT_FOUND` for a document that does not exist. |
| `/api/v1/inbox?project=&assignee=` | GET | human only | List open asks, newest first. `assignee=me\|unassigned\|<email>` keeps asks on issues held by the caller, by nobody (project-document asks included), or by that person (`400 ASSIGNEE_NOT_ALLOWED` when nobody has signed in with that email). |
| `/api/v1/me/answers?limit=&offset=` | GET | human only | The caller's answers and replies on asks, newest first. Answer rows come from user-authored `ask.answered` events whose actor is the answer's user, with one row per ask and answer timestamp; a settlement that restores an answered block adds none. Each row carries the ask's question, kind, state, `edited_at`, issue or project-document owner, deep link and either `answer` plus `current` or `reply`. `limit` is 1–200 (default 50), `offset` is 0 or more, and an invalid value is `400 INVALID_QUERY`. |
| `/api/v1/me/replies?limit=&offset=&issue=\|artifact=` | GET | human only | The threads the caller wrote in (an ask, a comment thread by its root, an issue message thread by its root) where someone else has written since the caller's read mark, newest reply first, less an open ask the Inbox lists, from `user_threads`. Each row carries `thread_id`, `kind`, `ref` (the thread's root), the issue or project-document owner, `subject` and the newest write by someone else as `reply`: its `id`, `kind` (`reply` for a comment or message, `answer` for an answer, whose `id` is the ask's and whose `first_line` is the chosen options and the answer's text), `author`, `first_line` and `created_at`. `issue` or `artifact` narrows to one owner and both is `400 INVALID_QUERY`; `limit` is 1–200 (default 50); `{rows, total, limit, offset}`. |
| `/api/v1/me/threads/{id}/read` | PUT | human only | Move the caller's read mark on one thread forward to `{read_through}`, stored as `least(value, now())`; more than a minute ahead of the server is `400 INVALID_STATE`, a non-uuid id `400 THREAD_ID_INPUT`. A thread the caller never wrote in writes nothing; `204` either way. A move appends `user_thread_state.updated`, which never notifies. |
| `/api/v1/agents` | GET | user or bearer | List live Envoy sessions with their `capabilities`, newest first, so a session can pick a target that advertises the delivery mode it wants. `api/agents.go` lists the stored roster every process shares (the `route_roster` row, read through `rosterCache` in `api/issue_route_status.go`): the listener's session list read through `internal/dispatch/envoy`, at most 5 s old by the database's clock and shared with issue reads' `route_status`; a failed listener read is `503 ENVOY_UNAVAILABLE`, kept for up to 5 s without asking the listener again, and a failed read of the row is `500 INTERNAL`. |
| `/api/v1/issues/{key}/asks` | POST | user or bearer | Create an ask. A quote anchor's mark carries the ask's id and who asked, which no rendering carries and every load of the document builds: the marks a document's comments, suggestions and asks hold on its text carry at most 1 MiB of ids and authors (`anchorWatch`), and an anchor that would leave them past it and carrying more is `413 CAP_EXCEEDED`, naming the bound and both sizes. |
| `/api/v1/issues/{key}/asks?state=&thread=` | GET | user or bearer | List an issue's asks, open and/or answered (`state`: `all` default, `open`, or `answered`). Each row is the ask plus `last_reply`, the newest comment in its thread (null when nobody has replied). `thread=1` adds `thread`, the `replies`, `edits`, `answers` and `followers` an ask card renders, batched over the whole list, so a page of asks costs no fetch per card; without it the row carries no `thread` at all, so the readers that render the ask alone (the review margin, a document's decision blocks) do not download every reply body on an unpaged list. Any other `thread` value is `400 INVALID_ASK_THREAD`. |
| `/api/v1/asks/{id}` | GET | user or bearer | Read an ask with its replies, edits, followers and `answers`: every user-authored answer, oldest first, with a settlement's restoration of an earlier answer collapsed. It carries `anchor_block`, where the anchor's block stands (its path, and in a table the row, column and header), when the live document holds it. When Dispatch cannot read the anchor's document the read still answers, without `anchor_block` and with `anchor_block_error`, the code the API answers that error with elsewhere: `DOC_SERVICE_UNAVAILABLE` (its room or store could not be reached), `DOC_SCHEMA` (its live tree is outside the schema), `DOCUMENT_UNLOADABLE` (its stored history cannot load; a rebuild restores it) or `INTERNAL`. |
| `/api/v1/asks/{id}` | PATCH | user or bearer | Edit one or more of `question`, `options`, `multiple`, or `urgency` while the ask is open. A bearer caller must be the asking session; a human may edit any open ask. The response has nullable `edited_at`; `ask.edited` records the full current ask, prior mutable fields, and `edited_by`. Ask anchors cannot be changed by this route. On a block ask the `:::ask` block is written in the same transaction and the row takes the block's parsed values, so the edit versions the document once and no settlement reverts it. Only the named fields are written: `urgency`/`multiple` alone go through the attribute path and leave the body's nodes, marks and inner block ids untouched, while naming `question` or `options` replaces that part with the markdown pipeline's own parse. A field named but unchanged is not rewritten, so an idempotent retry of the whole ask writes nothing, versions nothing, keeps every anchor and returns 200. Text the block cannot carry unchanged is `400 ASK_BLOCK_TEXT` naming the field, with nothing written. The text an edit writes is weighed before it is made into markdown - an element for each backslash, `*`, `_`, `~`, backtick, `[`, `]` and `<`, which its markdown escapes or reads as syntax, and two for each line feed (`pmdoc.WriteBudget.RefusePlainText`) - and its markdown is parsed on the write's element budget, so text past the 65,536 elements one write may make is `413 CAP_EXCEEDED` before it is rendered. An edit of a block ask that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, is `413 CAP_EXCEEDED` and the ask keeps its text. |
| `/api/v1/asks/{id}/answer` | POST | human only | Answer an open ask, or change the current answer with `expected_answer_at` set to that answer's `at`. Without it an answered ask is `409 ASK_CLOSED`. A stale value, or one sent for an open ask, is `409 ASK_ANSWER_CHANGED`; a different person is `403 NOT_ANSWERER`; an approval is `409 ASK_APPROVAL_REVIEW`. A change appends another `ask.answered` whose `previous_answer` is the replaced answer, so its asker and followers receive it through the same path as the first answer. An approval ask is moved to each document version before the human sees it, and an `ASK_EDITED` response means the question changed after the human reviewed it, so they reload and answer the moved request. On a block ask the answer is written into its block (`SetBlockAttributes`, through `applyLive`), and a changed answer without text removes the block's old `answer`. Its words and the options it chooses, whose labels are the asker's text, are caller text (`pmdoc.IsAnswerAttribute`): where they would leave the document's markdown longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, the ask still takes the whole answer and the block is written without them, saying who answered and when. Its state and who gave it and when are server state `refuseGrowth` does not weigh (`pmdoc.EqualOutsideServerState`), so the asks of a document already past those limits can still be answered. The answer is kept on the ask as well, and settlement writes it back into a block that leaves the document and returns, weighing each returning answer on its own in document order; one that would take the document past those limits and make it bigger stays on the ask alone and its block says who answered and when (`withholdAnswers`). |
| `/api/v1/asks/{id}/resolve` | POST | user or bearer | Retract or self-resolve an open ask with a recorded reason. On a block ask the block's `state` is written with it, so no later settlement reopens it or credits the repair to whoever next edits the document; being server state, it is not weighed, so an ask on a document past what one upload may hold can still be resolved. A reason beginning `removed from the document in version`, which marks a retraction settlement wrote, is `400 INVALID_RESOLUTION`. The kind is `retracted` or `resolved`, and any other is `400 INVALID_RESOLUTION`: `withdrawn` is the server's alone, written by an issue's close. An ask on a closed issue is `409 ISSUE_CLOSED`; reopen the issue first. |
| `/api/v1/issues/{key}/comments` | GET, POST | user or bearer | List or create comments and suggestions. A comment's margin record - its body and replies, or a suggestion's and its replacement - holds at most 256 KiB of text, and a document's margin 1 MiB: a comment, reply, suggestion or edit that would leave either past its bound and bigger is `413 CAP_EXCEEDED`, naming the bound and both sizes. A quote anchor that would leave the marks on the document's text carrying more than 1 MiB of ids and authors, and more than they carried, is `413 CAP_EXCEEDED` too, as for an ask's. A reply's `reply_to_block` names the block of the comment `reply_to` names that it answers, checked against that comment before `reply_to` climbs to its thread's head (`400 INVALID_COMMENT` without `reply_to`, for a block that comment does not hold now, and in an ask's thread); the delivery callback `POST /api/v1/comments/{id}/reply` takes it for a block of comment `{id}`. Every comment a route or event returns carries `reply_block` (`model.ReplyBlock`), `null` when it names no block. |
| `/api/v1/comments/{id}` | GET | user or bearer | Read a comment and its reply chain. It carries `anchor_block`, where the anchor's block stands (its path, and in a table the row, column and header), when the live document holds it. When Dispatch cannot read the anchor's document the read still answers, without `anchor_block` and with `anchor_block_error`, the code the API answers that error with elsewhere: `DOC_SERVICE_UNAVAILABLE` (its room or store could not be reached), `DOC_SCHEMA` (its live tree is outside the schema), `DOCUMENT_UNLOADABLE` (its stored history cannot load; a rebuild restores it) or `INTERNAL`. |
| `/api/v1/comments/{id}` | PATCH | human only | Edit a comment body, the author alone. `blocks` (`[{id, from}]`, UTF-16 offsets into `body`) with `base_revision` carries the dashboard editor's block ids (`itemblocks.Carry`); without them, or against a revision another save replaced, the ids follow the text (`itemblocks.Align`). `400 INVALID_COMMENT` naming the field for a bad carried id or `from` (`itemblocks.CarriedError`), or `blocks` without `base_revision`. |
| `/api/v1/comments/{id}/blocks` | GET | user or bearer | The comment's current blocks (`model.ItemBlocks`): `{item, revision, indexed, blocks: [{id, type, text, ref, from, to, changed_revision, changed_at}]}`, `from`/`to` UTF-16 code units of the stored body; `indexed: false` and no blocks while the comment is owed an index or was indexed from another body. The read writes nothing (`api/item_blocks.go`). |
| `/api/v1/comments/{id}/resolve` | POST | user or bearer | Resolve a comment. |
| `/api/v1/comments/{id}/accept` | POST | human only | Apply and accept a suggestion. An accept that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, is `413 CAP_EXCEEDED` and leaves the suggestion open. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and leaves the suggestion open; one removed after it answers `200` with `lost: true`. |
| `/api/v1/comments/{id}/reject` | POST | human only | Reject a suggestion. |
| `/api/v1/issues/{key}/messages` | POST | user or bearer | Post a short issue message. A reply's `in_reply_to_block` names the block of the message `in_reply_to` names that it answers (`400 INVALID_MESSAGE` without `in_reply_to` and for a block that message does not hold now); `POST /api/v1/agents/{session_id}/messages` takes it too, and `POST /api/v1/messages/{id}/reply` for a block of message `{id}`, refused on a follow-up. Every message a route or event returns carries `reply_block`, `null` when it names no block. |
| `/api/v1/issues/{key}/messages` | GET | user or bearer | List an issue's message threads: every root message, oldest first, unpaged, each with its delivery attempts and its replies, in the shape `/api/v1/issues/{key}/messages/{id}` answers. A targeted message is an ordinary root. |
| `/api/v1/messages/{id}?session=` | GET | user or bearer | Read the conversation a message belongs to, issue-less or not, by any message id in it: the thread root with its deliveries and every reply, oldest first. Who may read which thread is the route's description in `internal/dispatch/api/routes_table.go` and `packages/envoy/AGENTS.md` ("Targeted Dispatch messages"). |
| `/api/v1/messages/{id}/blocks?session=` | GET | user or bearer | The message's current blocks (`model.ItemBlocks`): `{item, indexed, blocks: [{id, type, text, ref, from, to}]}`, `ref` absent for a direct message's block, under `GET /api/v1/messages/{id}`'s read rule (`readableMessageThread`); `indexed: false` and no blocks while the message is owed an index. The read writes nothing. |
| `/api/v1/messages/{id}/deliveries/{attempt}/accept` | POST | bearer only | The attempt's session records that it took the message as its user's own turn, `{actor: {kind: "session", id}}`, and is answered the attempt with the message's stored `body` (`AcceptedMessageDelivery`); `403 ACCEPT_FORBIDDEN` for another session's attempt or a non-bearer. One compare-and-set under the message's row lock (`FOR NO KEY UPDATE`, which the session's reply's foreign-key `FOR KEY SHARE` does not wait for), after the event owner's lock. It succeeds only for a direct message to that session, on no issue, neither a broadcast's copy nor a reply in a broadcast's thread, in a thread whose root targets that session (`409 ACCEPT_NOT_DIRECT`); that a person wrote (`409 ACCEPT_NOT_WRITTEN_BY_PERSON`); sent as a Send or an Aside (`409 ACCEPT_NOT_ASIDE_OR_STEER` for a BTW); when no attempt of it was accepted before (`409 ACCEPT_ALREADY_ACCEPTED`); only the message's latest attempt (`409 ACCEPT_SUPERSEDED`), whose `requested_by` is a person (`409 ACCEPT_NOT_REQUESTED_BY_PERSON`; a row from before migration 0054 records nobody) and that person the message's author, logins compared case-insensitively (`409 ACCEPT_NOT_REQUESTED_BY_AUTHOR`: another person's retry), which Dispatch did not record as failed (`409 ACCEPT_FAILED`), created within the last minute by Postgres's clock (`409 ACCEPT_STALE`). A pending attempt may be accepted: the route writes only `accepted_at` and `accepted_as: "user_turn"`, never the state or claim the send settles, and appends `message.accepted` (`{message_id, attempt, session_id, accepted_as, target}`), never a second `message.delivery`; the event is issue-less, so the outbox never publishes it to NATS. The actor is the bearer's own claim, as on every session write. |
| `/api/v1/agents/{session_id}/messages` | POST | human only | Send one issue-less message to one session: `{body, delivery, in_reply_to?}`. Writes the message and, under an `Idempotency-Key`, the key's recovery point (the message and its mode) in one transaction, then makes its first attempt on a context detached from the request's, so a client that goes away cannot cancel it and a shutdown waits for it, and answers 201 with the attempt once it has settled, which keeps a person's sends to one session in press order. Keyed as every write is (above the table): a repeat under the key first puts the message on its way when it is owed (`owedMessageAttempts`: no attempt at all, or its latest attempt pending under a claim that has lapsed), then answers `201` and `Idempotency-Replayed: ?1` with the message and its deliveries as they stand, from the database before the listener is read; the key with a different request is `422 KEY_REUSED` and sends nothing. An attempt a dead process left owed is sent by the delivery sweep whether anyone asks again or not. The body field `idempotency_key` is gone. |
| `/api/v1/broadcasts` | POST | human only | Send one message to many sessions: `{body, delivery, session_ids}`. Writes the broadcast, one issue-less targeted message per recipient and, under an `Idempotency-Key`, the key's recovery point (the broadcast and its exclusions) in one transaction, answers 201, then delivers behind the request (four workers, each on its own tracking context detached from the request's, registered with the server's background wait so a shutdown waits for it, and bounded per recipient by the listener client's receipt timeout) through the ordinary targeted-message path, so neither a cut-off request nor a deploy can strand a recipient. Each recipient comes back with its first attempt pending. A selected session that is not live or does not advertise `delivery` is excluded and named in `excluded` (never switched to another mode); a selection with no reachable recipient is `400 BROADCAST_EMPTY` and writes nothing. At most 100 recipients (`contracts.MaxBroadcastRecipients`, generated from `MAX_BROADCAST_RECIPIENTS` in `packages/contracts`, which the dashboard also reads), duplicates collapsed. Keyed as every write is (above the table): a repeat under the key answers `201` and `Idempotency-Replayed: ?1` at once, with the broadcast read as it stands and the send's own `excluded`, from the database before the listener is read, so a repeat is answered while the listener is down, and puts every recipient the broadcast still owes on its way behind the answer (`owedMessageAttempts`; one a live claim holds is left to the sender holding it); the key with a different request is `422 KEY_REUSED` and sends nothing. A recipient a dead process left owed is sent by the delivery sweep whether anyone asks again or not. The body field `idempotency_key` is gone. |
| `/api/v1/broadcasts` | GET | human only | List the 50 newest broadcasts with their recipient and reply counts. |
| `/api/v1/broadcasts/{id}` | GET | human only | Read one broadcast with every recipient's message, delivery attempts and replies. Recipients come back in the order the send named them, after exclusions; a broadcast written before migration 0052, or by an older server during a rollout, has no stored order and falls back to `created_at, id`. Unknown or malformed id is `404 BROADCAST_NOT_FOUND`. |
| `/api/v1/issues/{key}/artifacts` | GET, POST | user or bearer | List issue artifacts or create a version from a multipart file or JSON inline content. The JSON form requires `Content-Type: application/json`. A file's kind is read from its bytes (`artifactKind`, above): `image` with `mime` the type its bytes are only for a PNG, JPEG, GIF or WebP, whatever the part declared, otherwise a `file` under the declared type; a version whose kind is not its artifact's is `400 ARTIFACT_KIND_MISMATCH`. An ask block whose body breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes. A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose markdown makes more than 65,536 elements (weighed as `README.md`'s Element weights lists), naming the line that passes the limit; a document or version whose markdown as stored (what `.../text` answers) would be longer than 1 MiB and longer than the document's was, or make more than 65,536 elements and more than the document's did, or that would leave a live state ygo could not decode as one update; or whose formatting is more items than one document update can store (1,048,576), naming the count. A file the configured bucket refuses is `502 FILE_STORE_UNAVAILABLE`, with no version written. A fresh POST to a closed or missing issue is refused as its handler would refuse it (`409 ISSUE_CLOSED` for a closed one) before its body is spooled, keyed or not, and claims no key (`refuseUploadToAClosedIssue`, which `register` mounts ahead of the key layer; the handler's own locked check still decides). It stands in for the handler, so a repeat under a key the key layer answers itself (`requestKeyAnswersItself`: a stored answer, a first still running, a write that committed and lost its answer) goes on to that layer whatever the issue's state, and an upload that landed before the close is replayed its `201`; the key is looked up only when the issue refuses with its own 4xx. A malformed `Idempotency-Key` is left to the key layer's `400 KEY_INVALID`, which spools no body either. The body of every such early answer is read off the connection and discarded first (`drainBeforeAnswer`). |
| `/api/v1/artifacts/{id}` | GET | user or bearer | Read an artifact, versions, and incoming references. `{id}` must be a UUID. Every version every artifact read, list, version write and `artifact.*` event carries has `size`, the bytes it stores (a document's markdown, a file's or an image's bytes), selected as `model.VersionSize`, which reads a document row's markdown length with `octet_length` and never renders the document or loads its history. |
| `/api/v1/artifacts/{id}/rebuild` | POST | human only | Rebuild a document whose stored history cannot load (`409 DOCUMENT_UNLOADABLE`) from its latest saved version or supplied `{markdown}`, in one transaction with the version and event supplied markdown writes and the move of an open approval request to that version. A resident room is `409 DOCUMENT_LIVE`, a history that loads `409 DOCUMENT_LOADS`, supplied markdown on a closed issue `409 ISSUE_CLOSED`, and every refusal comes before anything is written. Supplied markdown is parsed as an upload is and its rendering weighed as a new document's (`SeedText`): past 65,536 elements, or rendering longer than 1 MiB or making more than 65,536 elements, it is `413 CAP_EXCEEDED`. The latest version is the server's own rendering, read back counting no elements (`pmdoc.ParseRendering`), so it rebuilds whatever it weighs. The report's `source_version` is the version the rebuilt document holds. |
| `/api/v1/artifacts/{id}/text` | GET | user or bearer | Read a live document's markdown. `{id}` must be a UUID. A stored tree outside the Proof schema is `409 DOC_SCHEMA`, which `/blocks`, `POST /edits` and `POST /versions` answer alike with one message; a stored history that cannot load is `409 DOCUMENT_UNLOADABLE`, which `POST /api/v1/artifacts/{id}/rebuild` repairs. |
| `/api/v1/artifacts/{id}/blocks` | GET | user or bearer | A document's blocks with markdown ranges, tokens, and reference counts; a table counts its cells' anchors and lists its rows and cells (`table.rows`: each row's id and index, each cell's id, row, column, header and opening words), placed as the block route places them. |
| `/api/v1/artifacts/{id}/blocks/{block_id}` | GET | user or bearer | Where one block stands in a document: its path from the top-level block down (type, id, child index), and for a table block, row or cell the table's id, the row index (0 is the header), the cell's column index, the text of the header cell drawn above it (colspans and rowspans placed) and the row's cells; `404 TARGET_NOT_FOUND` for an id the live document does not hold. |
| `/api/v1/artifacts/{id}/versions/{n}` | GET | user or bearer | Read a document version or download a blob. `{id}` must be a UUID. A file whose bytes are in the bucket is `502 FILE_STORE_UNAVAILABLE` when the bucket does not answer, `503` on a server with no bucket configured. A file or image version carries `Cache-Control: private, max-age=31536000, immutable`; a document version, and every error, carries none. |
| `/api/v1/artifacts/{id}/versions` | POST | user or bearer | Create a named live-document version. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/edits` | POST | user or bearer | Apply document edit operations. `{id}` must be a UUID. An edit is `400 INVALID_ASK_BLOCK` when an ask it writes or changes breaks its content rule (`paragraph+ bullet_list?`) or holds what settlement cannot read; an ask it carries through unchanged is not its to refuse. A batch whose markdown makes more than 65,536 elements, or that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, or a live state ygo could not decode as one update, is `413 CAP_EXCEEDED` with the refusal itself as the error, and writes nothing. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/artifacts/{id}/approval-requests` | POST | user or bearer | Ask a human to approve a document's latest settled version. The question is `Approve <name> (version <N>)?` followed by the optional `summary` (blank is `400 SUMMARY_INPUT`; past the ask cap at the longest version a request can reach, ten digits, is `400 CAP_EXCEEDED` naming `summary`). A request on a document a human approved earlier names that approval in `ask.approval.approved` (`{version, at, by}`); a first request carries no `approved` key. One open approval row follows every document version in place, rewording its question and emitting `ask.edited`; while its `requested_version` is below the new version it waits on the agent. Only the first move since the request was opened or handed back notifies, even when a thread reply had already left it waiting on the agent; a later move, while `requested_version` is already below the version it named, is `quiet: true`, `notify: false`, and reaches no follower. A version a human's answer to a decision block writes, when that answer is the whole change since the version he approved (`docs.carryApproval`; who answered what is the document's asks rows, and an ask block no row of the document backs, a copy, one settlement could not read, one whose server-owned attributes are not as settlement writes them from its row, or a block id the version repeats carries nothing), carries his approval to it in the same transaction - an `approved` review in his name whose `reason` names the carry, and `artifact.approved` - and answers the open request `Approve` in his name after the move (`ask.answered`, the review's `ask_id`), so nothing is requested again for it. Calling this route again while the row waits on the agent - moved, or a thread reply newer than its last hand-back holds the turn - hands it back to the human: a summary that changes its question rewords it first (`ask.edited`; an omitted summary keeps its prior one), then `requested_version` becomes the latest version and `ask.handed_back` is emitted, leaving `edited_at` as it was. While the row waits on the human, the same summary or none is a repeat that answers `200` with no event, and a different one is `409 APPROVAL_WAITS_ON_HUMAN` and changes nothing, since it would rewrite the card the human is reading. It answers `201` when it wrote anything, with the document's `approval` as the call left it (`waiting_on` while awaiting). |
| `/api/v1/artifacts/{id}/asks?state=` | GET, POST | user or bearer | List or create asks on an unlinked document. |
| `/api/v1/artifacts/{id}/comments` | GET, POST | user or bearer | List or create comments and suggestions on an unlinked document. |
| `/api/v1/artifacts/{id}/events` | GET | user or bearer | Read an unlinked document's events. |
| `/api/v1/artifacts/{id}/references` | GET | user or bearer | Read outgoing and incoming reference edges. `refs.Outgoing` lists the document's mentions, a block target with its document as `artifact` while a current version holds the block; `refs.ReferencedBy` lists each item and document citing the document or one of its current blocks once, a document as itself. |
| `/api/v1/issues/{key}/artifacts/{slug}` | GET | user or bearer | Read an issue artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/issues/{key}/artifacts/{slug}/text` | GET | user or bearer | Read an issue artifact's live markdown. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions/{n}` | GET | user or bearer | Read an issue artifact version or download its blob, as `/api/v1/artifacts/{id}/versions/{n}` does. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions` | POST | user or bearer | Create a named issue-document version. |
| `/api/v1/issues/{key}/artifacts/{slug}/edits` | POST | user or bearer | Apply issue-document edit operations. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/projects/{key}/artifacts/{slug}` | GET | user or bearer | Read an unlinked project artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/projects/{key}/artifacts/{slug}/summary` | GET | user or bearer | What a reference to a project document shows, read without its versions: `{id, project, slug, name, kind, latest_version}`, `{slug}` resolved as the project document read resolves it (`404 ARTIFACT_NOT_FOUND`, `400 ARTIFACT_REFERENCE_AMBIGUOUS`). |
| `/api/v1/projects/{key}/artifacts/{slug}/text` | GET | user or bearer | Read an unlinked project document's live markdown. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions/{n}` | GET | user or bearer | Read an unlinked project document version or download its blob, as `/api/v1/artifacts/{id}/versions/{n}` does. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions` | POST | user or bearer | Create a named project-document version. |
| `/api/v1/projects/{key}/artifacts/{slug}/edits` | POST | user or bearer | Apply project-document edit operations. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/agents/{session_id}/artifacts` | POST | user or bearer | Upload a file or image to an agent's conversation (multipart, an issue upload's caps, errors, kind read from the bytes and file-store write; no event). There is no list route: the conversation's pictures are read by the references the messages carry. A caller whose credential proves a session of its own uploads only into that session: any other `session_id` is `403 SESSION_SCOPE` (`api.withinSessionScope`). A markdown document is `400 ARTIFACT_INPUT`: an agent's conversation holds files and images, and a document belongs to an issue or a project. Such a file's comments, asks, events and subscribers under `/api/v1/artifacts/{id}/...` are `400 ARTIFACT_AGENT_OWNED`. A `session_id` `text.IsSessionID` refuses is `400 INVALID_SESSION_ID` on every agent artifact route. |
| `/api/v1/agents/{session_id}/artifacts/{slug}` | GET | user or bearer | Read an agent's conversation's file or image and its versions by slug. |
| `/api/v1/agents/{session_id}/artifacts/{slug}/versions/{n}` | GET | user or bearer | Download one version of it, as `/api/v1/artifacts/{id}/versions/{n}` does. |
| `/api/v1/me/state` | GET | identity | Read the user's issue UI state. |
| `/api/v1/me/issues/{key}/state` | PUT | identity | Update the user's issue UI state. |
| `/api/v1/me/agents/state` | GET | identity | Read the user's per-agent conversation state: `{[session_id]: {cleared_before?, read_through?, unread_replies}}`, where `unread_replies` counts the session's replies to the user's direct messages newer than both the Clear and the read mark and not read by id. |
| `/api/v1/me/agents/{session_id}/state` | PUT | identity | Clear an agent's conversation (`cleared_before`), mark it read (`read_through`, which only moves forward and covers every reply up to it), and/or mark some of its replies read by id (`read_replies`, messages the path's session wrote, covering those alone, each in any form `uuid.Parse` reads) for this user: at least one; 400 `INVALID_STATE` when none is given, a cutoff is malformed or more than a minute ahead of the server clock, or a `read_replies` id is not a uuid or not a message that session wrote (nothing is written then); an accepted cutoff is stored no later than the server's now. Appends `user_agent_state.updated` (`{login, session_id}`) for the viewer's other devices, except for a request naming only replies already read by id or passed by the read mark, which changes nothing (a reply the Clear hides but the read mark has not passed is neither: the first request naming it writes its row and appends the event), and answers the session's state. |
| `/api/v1/artifacts/_test/quiesce` | POST | user or bearer, `DISPATCH_TEST_HOOKS=1` only | Close every live document on every process and wait for the settlements in flight, answering once every process has; not mounted otherwise. |
| `/api/v1/live/_test/disconnect` | POST | user or bearer, `DISPATCH_TEST_HOOKS=1` only | Close every open `/ws/live` socket on every process with 1012 (service restart); not mounted otherwise. |
| `/api/v1/live/_test/rotate` | POST | user or bearer, `DISPATCH_TEST_HOOKS=1` only | Close every open `/ws/live` socket on every process with 4001 (`max age`); not mounted otherwise. |
| `/api/v1/agents/{session_id}/stream/_test/publish` | POST | human only, `DISPATCH_TEST_HOOKS=1` only | Feed every process's in-memory agent relay: one frame, `?as=replay` what a new agent view is answered with, `?as=responder` whether anyone answers on the control subject, `?as=drop` ends every open agent view as `dropped`, so the page subscribes again and rebuilds from that replay; not mounted otherwise. |
| `/api/v1/writes/_test/slow` | POST | user or bearer, `DISPATCH_TEST_HOOKS=1` only | `{method, path, delay_ms}`: every keyed run of that method and exact path waits `delay_ms` on its handler's context once its key is claimed (`503 SLOW_WRITE_CUT_OFF` when that context ends first); `delay_ms: 0` removes it; it arms the process the call reached, not every process; not mounted otherwise. |
| `/ws/doc/{room}` | GET | user | Join the Hocuspocus document room. The room takes a signed-in person's cookie and no bearer at all (INFRA-264): the editor is the browser's, and nothing else connects. A room whose stored tree is outside the Proof schema admits no connection, a provider's reconnect included: the upgrade completes and closes with code `4409` (reason `DOC_SCHEMA`; `DOCUMENT_SCHEMA_CLOSE_CODE` and `DOCUMENT_SCHEMA_CLOSE_REASON` in `packages/contracts`) before any sync, and the dashboard reads that close as the repair state. The check reads a copy of a resident room taken under its lock, as `/text` reads it, so a healthy room under writes is never refused. An open socket resolves its caller again on every 15 s heartbeat, as `/ws/live` does, and closes once they no longer resolve (a sign-out, a membership the sign-in issuer no longer confirms), so their editor stops receiving and sending the document's edits. ygo echoes a read-write peer's awareness renewal to its sender and gives a read-only peer a bounded empty awareness response, so an idle provider remains connected without Dispatch writing a keep-alive frame. |
| `/ws/live` | GET | user or bearer | One live socket per browser tab. Each `subscribe` names a delivery, `events`, or human-only `agent` view; the socket replies on that subscription id with delivery snapshots/deltas/freshness, `events_start` and events, or agent responder/replay/frame data. An `events` resume is its decimal event id, and its `events_start` says `replay_complete: true` when that id is at or below the log's head, so every event after it is replayed before the subscription goes live, and `false` with no resume or one above the head; a delivery resume is its change and revisions; an agent view never resumes. Every quiet socket receives a `heartbeat` every `StreamHeartbeat` (15 s). It closes with 4001 (`max age`) at a random age in [30, 60) minutes, 1012 on restart, 1008 when the caller stops resolving, and 1013 when its hub queue fills. |

## Dispatch topics

Document events publish retained envelopes on
`notifications.dispatch.document.<PROJECT>.<slug>.<type>`. `natstail` reads
`envoy.natsUrls` from `envoy.json` and prints a matching envelope:

```sh
go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1
```

`natstail` publishes nothing and owns nothing on the bus: it neither creates nor
updates `ENVOY_NOTIFICATIONS`, and it refuses a NATS server that is not this
machine's, naming the URL. A machine whose `envoy.json` names a shared NATS
(an agent devbox names production's) runs it with
`ENVOY_ALLOW_REMOTE_NATS=1 go run ./cmd/natstail …`, which is the run saying it
means that server. It connects as the nkey user `NATS_NKEY_SEED_FILE` (or
`NATS_NKEY_SEED`) names, never `envoy.natsSeed`, so a run against a NATS that
requires a user sets `NATS_NKEY_SEED_FILE` too.

## Checks

```sh
cd packages/envoy
export DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable'
go test -timeout 20m ./internal/dispatch/api
go test -timeout 20m $(go list ./internal/dispatch/... ./cmd/dispatch/... | grep -v '/internal/dispatch/api$')
go vet ./...
```

`internal/dispatch/api` alone takes close to `go test`'s default ten-minute package deadline (about
600 s), so it runs first and on its own, with 20 minutes, as CI's `envoy-go` job runs it
(`docs/testing.md`, "Go's package deadline").

The real-process memory tests (`memory_test.go`, LEGION-481) start a Dispatch server per row and
read its resident memory from `/proc`, so they build only under the `memory` tag and run in their
own CI job (`dispatch-memory` in `.github/workflows/ci.yaml`), past `go test`'s
default ten-minute deadline:

```sh
env DISPATCH_TEST_DATABASE_URL=… go test -tags memory -count=1 -timeout 60m ./cmd/dispatch
```

The `dispatch-memory` job runs only when a file its `dispatch_memory` filter in
`.github/workflows/ci.yaml` names changes. `cmdtest.Build` builds `./cmd/dispatch`
itself, so every package the server imports is in that filter; `packages/envoy/AGENTS.md` (Critical
conventions) says what keeps it current.

The two-process tests (`multiprocess_test.go`, INFRA-66, INFRA-555) start two real Dispatch servers on one
Postgres and one NATS and drive the properties a rolling release depends on — no two passes of a
leased loop at once, one pass per period across a leased timer loop, a woken loop's held lease
retried until each process has run its own pass, the delivery intake's standby and takeover, each
outbox row published once, an event-feed subscription receiving another process's event, a
document room closed when the other process writes it, a sign-in started on one process completed
on the other, people kept signed in across a key rotation, a process whose key set a live peer
does not share refused at boot, `server.started` published once per fleet-wide build, every
dashboard asset of either of two builds served by both processes, and the builds
`GET /api/v1/dashboard/builds` names as live. They build only under the `multiprocess` tag and run
in their own CI job (`dispatch-multiprocess` in `.github/workflows/ci.yaml`), because two servers
and their NATS take minutes rather than seconds:

```sh
env DISPATCH_TEST_DATABASE_URL=… go test -tags multiprocess -count=1 -timeout 30m ./cmd/dispatch
```

The `dispatch-multiprocess` job runs only when a file its `dispatch_multiprocess` filter in
`.github/workflows/ci.yaml` names changes; `packages/envoy/AGENTS.md` (Critical
conventions) says what keeps it current.

A developer who runs only the block above runs neither tagged suite, which is why both are named
here: the evidence for anything about two processes serving one database is in the second one.
