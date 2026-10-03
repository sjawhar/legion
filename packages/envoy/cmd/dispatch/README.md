# Dispatch HTTP server

Go binary serving the Dispatch dashboard, GitHub OAuth sign-in, and the
per-user GitHub REST and GraphQL proxy. Dispatch stores user OAuth tokens and
application state in Postgres.

## Required configuration

Every environment variable the server and its subcommands read is a row of one table,
`cmd/dispatch/settings.go`: `main` reads each row once, every reader takes its value from that
read, and a test fails on any other environment read in `cmd/dispatch` or `internal/dispatch`.
`envoy-dispatch settings` prints the table (name, `_FILE` form, default, whether it is required,
and a one-line description) without a database or a listener, and the docs site's Dispatch
configuration reference is generated from it. The tables below explain the settings that need
more than a line.

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Postgres connection string. Dispatch applies embedded migrations before serving. The pool size is fixed in code (`store.sharedPoolSize`), so a connection string carrying `pool_max_conns` is refused at startup; remove the parameter. |
| `DISPATCH_SERVER_URL` | Public browser origin. When set, overrides `dispatch.serverUrl` from merged `envoy.json`. |
| `NATS_URLS` | Comma-separated NATS URLs. When set, overrides `natsUrls` from merged `envoy.json`. |
| `NATS_NKEY_SEED_FILE`, `NATS_NKEY_SEED` | The NATS nkey user Dispatch connects as: a file holding the seed (trimmed; wins), or the seed. A set but unusable value refuses startup naming the variable and path; neither set connects without a credential. |
| `DISPATCH_AGENT_TOKEN` | Shared bearer fallback for devbox agents. Personal tokens minted in Settings are the normal agent credential. |
| `DISPATCH_ALLOWED_LOGINS` | Comma-separated GitHub login allowlist. Required for cookie identity mode and enforced during OAuth sign-in. |
| `DISPATCH_TEST_HOOKS` | Set to `1` to mount `POST /api/v1/events/_test/disconnect`, which closes every open SSE connection, and `POST /api/v1/artifacts/_test/quiesce`, which closes every live document and waits for the settlements in flight. Test/e2e only — leave unset in every real deployment. |
| `DISPATCH_DEV_SIGNIN` | Set to `1` to mount `GET /auth/_dev/signin?login=<login>&next=<path>`, which signs an allowlisted login in with no GitHub step, so a browser or test harness can be signed in to a local instance. Boot refuses it unless identity is `cookie`, the listen address is a loopback IP literal, the dashboard origin (`DISPATCH_SERVER_URL` or `dispatch.serverUrl`) names `127.0.0.1`, `[::1]` or `localhost`, every `DATABASE_URL` host is loopback or a unix socket, `DISPATCH_SIGNING_KEY` is unset, `ENVOY_ALLOW_REMOTE_NATS=1` is not set while NATS is on, `DISPATCH_AGENT_SECRETS_URL`, when set, names a loopback host, `ENVOY_URL` names a loopback host, and a loaded GitHub App private key comes from `DISPATCH_APP_PEM_B64` with `DISPATCH_GITHUB_API_BASE` naming a loopback host, never from the `pem` in `app.json`, where a developer keeps the real App's key: a signed-in session can have the App probe and import any repository it is installed on. That key must be a throwaway, as `packages/dispatch/e2e/run-server.sh` generates one, since every App call hands a signed App JWT to whatever listens at that base. While it is on every request must carry the dashboard origin as its `Host` (else `421 HOST_MISMATCH`), and the GitHub proxy answers `503 GITHUB_TOKEN_UNAVAILABLE` for every login. The session cookie is signed with a key generated for that process alone, so it is worthless on any other server; what a signed-in session writes to the database is not. It can mint a `dsp_` personal agent token, and its sign-out advances the login's session generation and deletes its stored GitHub token pair, and every server on the same database honours those rows. Give a dev-sign-in server a database no other server uses: the loopback check makes that likely, not certain, since a loopback address can be a tunnel to another machine's database or a database a second local server also runs on. Any value other than `1` or unset is refused. |
| `ENVOY_URL` | Base URL of the Envoy listener (`GET /v1/sessions`) behind `GET /api/v1/agents`; defaults to `http://127.0.0.1:9020`. Must name a loopback host with `DISPATCH_DEV_SIGNIN=1`. |
| `DISPATCH_OIDC_ISSUER` | OIDC issuer whose projected service-account tokens authenticate as agents. Set with `DISPATCH_OIDC_AUDIENCE` or not at all. |
| `DISPATCH_OIDC_AUDIENCE` | Audience those tokens must carry (`dispatch`). Set with `DISPATCH_OIDC_ISSUER` or not at all. |

`DISPATCH_REPO_PROJECTS` optionally seeds repository-to-project settings at boot
with comma-separated `owner/repo=KEY` entries. Existing dashboard mappings take
precedence over this seed. Dispatch resolves every external issue through the
stored mapping, then falls back to `DISPATCH_DEFAULT_PROJECT` when configured.
An unmapped external repository without a default project is rejected. An issue
created through the default also gets a `repo:owner/name` label.

`DISPATCH_SERVER_URL`, when set, overrides `dispatch.serverUrl` in merged
`envoy.json`. It must be an absolute `http` or `https` URL with no path.
`NATS_URLS`, when set, overrides `natsUrls` with its comma-separated values.

`DISPATCH_SERVER_URL` IS the GitHub OAuth callback origin. It must equal the
URL humans type into their browser, and the GitHub App must list
`<DISPATCH_SERVER_URL>/auth/callback` as its callback URL.

Agents normally authenticate with a personal `dsp_` token minted in Settings,
sent as `Authorization: Bearer <token>`. `DISPATCH_AGENT_TOKEN` remains the
shared devbox fallback and does not attribute callers to an owner.

With `DISPATCH_OIDC_ISSUER` and `DISPATCH_OIDC_AUDIENCE` set, a Kubernetes pod's
projected service-account token is a third bearer credential: the server reads
the issuer's discovery document at boot (an unreachable issuer refuses to start,
naming it) and verifies a JWT-shaped bearer against its published keys, the
configured audience, and the token's expiry. Half the pair refuses to boot,
naming the missing variable; neither leaves bearer handling exactly as it is
without them. A verified caller acts as the `session` it names in the request
body, and every actor it writes carries `service`, the token's verified subject
(`system:serviceaccount:<namespace>:<name>`) — set from the token, never from
the body. A JWT the verifier rejects is `401 OIDC_TOKEN_INVALID` naming the
reason class; it is never retried as a personal token.

GitHub App credentials come either from these environment variables or from
`~/.local/share/dispatch/app.json`; environment variables take precedence:

| Variable | Purpose |
| --- | --- |
| `DISPATCH_APP_CLIENT_ID` | GitHub App OAuth client ID. |
| `DISPATCH_APP_CLIENT_SECRET` | GitHub App OAuth client secret. |
| `DISPATCH_APP_PEM_B64` | Base64-encoded GitHub App private key. With `DISPATCH_DEV_SIGNIN=1` this is the only source a key may come from (a `pem` in `app.json` is refused), it must be a throwaway, and `DISPATCH_GITHUB_API_BASE` must name a loopback host. |
| `DISPATCH_GITHUB_API_BASE` | GitHub API origin override for App calls (tests and e2e point it at a fake); empty means `https://api.github.com`. With `DISPATCH_DEV_SIGNIN=1` and an App private key loaded, it must name `127.0.0.1`, `[::1]` or `localhost`. That checks the host, not what listens there: every App call hands a signed App JWT to whatever owns the port, so the key must be a throwaway. |
| `DISPATCH_SIGNING_KEY` | Stable HMAC key for cookie sessions. Must be unset with `DISPATCH_DEV_SIGNIN=1`. |

When no GitHub App credentials are configured, the server still starts, but
OAuth and GitHub proxy routes respond with `503`, and saving a project's
architecture source answers `409 SOURCE_ACCESS` naming the missing key.

## Webhook redelivery

GitHub does not redeliver a webhook delivery that failed. When NATS is configured and the App's
private key (`DISPATCH_APP_PEM_B64`) is present, Dispatch redelivers them itself, following GitHub's
documented approach. Every two minutes it lists the App webhook's attempts
(`GET /app/hook/deliveries`) and judges each delivery by its GUID, as GitHub's own redelivery
script does: a GUID with an `OK` attempt is delivered, whatever its failures, and it asks GitHub to
redeliver each other GUID with an attempt whose status is not `OK`
(`POST /app/hook/deliveries/{id}/attempts`). Both endpoints accept only an
App JWT, which is why Dispatch runs this. The webhook the App delivers to is the Envoy listener's
`/webhook/github`. A redelivery carries the original `X-GitHub-Delivery`, and the listener
publishes GitHub envelopes under a JetStream MsgId of it. So redelivering a delivery that did
reach the stream adds nothing to the stream.

- A delivery the listener answered with 5xx, or that GitHub could not complete, is redelivered
  on the next sweep. If that redelivery fails too, it is retried after 2, 4, 8 and 16 minutes, at most
  five times.
- A redelivery request GitHub refuses (for example 422) delivers nothing. It is asked again on
  the same backoff, at most five times, even after its failed attempt is older than the sweep's
  hour: a refused request adds no attempt to GitHub's log, so the sweep's record brings it back.
- A rate-limited answer (403 or 429, as GitHub's REST rate-limit documentation describes one)
  stops the sweep and counts nothing against the delivery. No sweep sends GitHub anything until
  the time GitHub gave (`Retry-After`, or `x-ratelimit-reset` when no requests remain), and at
  least one minute, doubling while limits follow one another. Each sweep reads the recorded limit
  again right before every request it sends, so one limit stops every sweeper at its next request;
  a request already sent when another sweeper records a limit is not recalled. It logs
  `level=WARN msg="webhook redelivery rate-limited by GitHub"`. Redelivery requests go a second
  apart, as GitHub asks of a large number of POSTs.
- A delivery the listener answered with 4xx is never redelivered: the listener refused the
  request itself, and the same bytes would fail the same way.
- A delivery GitHub has recorded an `OK` attempt for is never asked for again, even when a request
  for it was recorded as refused (an accepted request whose answer was lost).
- Giving up logs `level=ERROR msg="webhook redelivery exhausted"`. A 4xx logs
  `level=ERROR msg="webhook delivery refused terminally"`. Each is logged once per delivery.
  Every sweep logs an INFO `msg="webhook redelivery sweep"` line: `failed_attempts` (the failed
  attempts listed), `deliveries` (the deliveries the sweep decided: the GUIDs of the listed failed
  attempts, plus refused deliveries the listing no longer returns), and one count per outcome,
  which add up to `deliveries`. `closed` is a delivery an earlier sweep settled (GitHub recorded it
  `OK`, it was refused terminally, or it was given up on): its failed attempt stays listed until it
  is older than the sweep's hour, so after a burst of failures is redelivered, the next hour's
  sweeps count the burst as `closed`.
  `pending` is a redelivery GitHub accepted and has not made yet.
- A sweep looks back one hour. After a gap in sweeping (Dispatch down), it resumes from its
  cursor, back to GitHub's three days. The first sweep ever made looks back one hour only.

The cursor, the last rate limit, and one record per delivery acted on live in the JetStream KV
bucket `envoy_webhook_redelivery`. Its 72-hour TTL matches GitHub's horizon. A compare-and-swap
claim keeps two Dispatch processes from requesting the same attempt twice, and the operator
command honours the same rate limit.

The operator command runs the same sweep over a chosen window. It does not move the running
sweep's cursor. `--dry-run` lists every failed delivery and what the sweep would do with it,
and requests and writes nothing:

```bash
envoy-dispatch redeliver-webhooks --since 72h --dry-run   # list what would be redelivered
envoy-dispatch redeliver-webhooks --since 72h             # redeliver under the sweep's rules
```

It reads the same App credentials and NATS configuration as the server, but owns nothing on the
bus: unlike the server it neither creates nor updates `ENVOY_NOTIFICATIONS`, and it refuses a
NATS server that is not the machine it runs on, naming the URL. Run inside the Dispatch
container it reaches that deployment's NATS with `ENVOY_ALLOW_REMOTE_NATS=1
envoy-dispatch redeliver-webhooks …`.

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

`DISPATCH_DEV_SIGNIN=1` adds a second way to get the cookie on a local
instance: `GET /auth/_dev/signin?login=<login>` checks the allowlist as the
OAuth callback does (lowercase, minting the spelling requested) and issues the
same session cookie with no GitHub exchange. The GitHub proxy then answers
`503 GITHUB_TOKEN_UNAVAILABLE` for every login, even one with a stored token
pair. The route serves only a loopback peer whose request carries no forwarding
header, logs every mint at WARN, and boots only behind the fence the
configuration table lists. A request that reaches the process looking local
(`ssh -L`, `socat`, a proxy that rewrites `Host` and adds nothing) is
indistinguishable from one that is. The per-process key bounds the cookie, not
every credential a dev session can write: a `dsp_` token it mints, and the
session and token rows its sign-out changes, live in the database, and every
server that reads that database acts on them. A dev-sign-in server therefore
needs a database of its own.

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

Then run (or re-run) the server. `DISPATCH_NATS_DISABLED=1` keeps this run off
the bus entirely; drop it and set `NATS_URLS` to a NATS of your own to exercise
the publish path. Neither line is optional decoration: without one of them the
server reads `natsUrls` from your `~/.config/opencode/envoy.json`, which on an
agent machine names the shared production server, and it would reconcile that
server's `ENVOY_NOTIFICATIONS` stream on the way in. It refuses to start against
a NATS that is not this machine's unless `ENVOY_ALLOW_REMOTE_NATS=1` says the
run means it.

```sh
cd packages/envoy
DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable' \
DISPATCH_AGENT_TOKEN=local-agent-token \
DISPATCH_IDENTITY='header:X-Dispatch-User' \
DISPATCH_ALLOWED_LOGINS=sjawhar \
DISPATCH_INSECURE_COOKIE=1 \
DISPATCH_DEFAULT_PROJECT=LOCAL \
DISPATCH_NATS_DISABLED=1 \
go run ./cmd/dispatch
```

The default listen address is `:8766`. Set `DISPATCH_LISTEN_HOST` and
`DISPATCH_PORT` to change it; `DISPATCH_LISTEN_HOST` may be an IPv6 literal
with or without brackets (`::1` or `[::1]`). A bad `DISPATCH_PORT` refuses the
boot before anything connects, and an address the server cannot bind ends the
process with exit status 1.

## Database migrations

Boot applies `internal/dispatch/store/migrations/*.up.sql` in version order and records each
version in `schema_migrations`, which it creates under the runner's advisory lock, so processes
booting together against an empty database migrate one after the other. Version 8 is an empty
file: that version was recorded from Go by the one-time conversion of pre-Proof `Y.Text` rooms and
offset anchors into Proof trees and mark anchors, which every deployed database has already run.

The runner records a migration by its version alone, so before it touches the database it reads
the whole directory (`pgmigrate.Load`) and refuses to start, applying nothing and naming every
file concerned, when two migrations share a version (it would apply the first and skip the rest as
already applied; keep the number on the file that merged to `main` first and renumber the rest),
when a file is named other than `<version>_<name>.up.sql`, `<version>_<name>.down.sql` or
`<version>_<name>.census.sql` (a `.down.sql` is a rollback script an operator runs by hand, and a
`.census.sql` is the migration's census, "Pre-deploy census" below; each needs its `.up.sql`),
when a census is not one `select` or calls a function it may not (below), when a version
is not decimal digits from 1 to 2147483647, and when a file cannot be read. The directory is
embedded with `all:`, so a name beginning with `_` or `.` is refused like any other, and an editor's
swap file left in the directory fails a local build's tests until it is gone. Versions are applied
by number, not by file name, and the store's tests require every file on disk to be embedded
(`TestEveryMigrationFileIsEmbedded`) and the versions to run 1 to N with no gap
(`TestMigrationSetIsNumberedOneToN`), both reading file names alone, so take the next free number
on `main`.

Every migration's lock waits are bounded at five seconds (`pgmigrate.LockTimeout`, which
`pgmigrate.Exec` sets on each migration it applies, after the runner's advisory lock), so a
migration queued behind a long transaction fails the boot instead of holding every read and write
of its table behind its request. The failure names the migration, the lock it wanted, the sessions
it was queued behind, and the `pg_stat_activity` query that lists the holders; end the holder or
let it finish and start the server again. A migration that needs another bound sets its own
`SET LOCAL lock_timeout`.

Migrations `0056`–`0062` make search indexing linear. Each table has its own migration, so its
transaction holds an `ACCESS EXCLUSIVE` lock only for the `DROP EXPRESSION` and trigger setup;
`0062` re-indexes only rows holding sixteen or more underscore-joined segments under `ROW EXCLUSIVE`.
Their censuses answer `0` for 0056–0061, which neither refuse nor rewrite a row, and, for 0062,
the candidate rows whose stored vector the new expression changes.

Migration `0063_doc_settlements_pending` creates the table in which every durable document update
records the settlement it owes, which the settlement deletes when it commits. A room's load and the
server's minute-by-minute resumption arm the settlement a row names, so one a shutdown cuts short
still runs. It creates a table and touches no row; its census answers `0`.

Migration `0009_project_artifacts` deletes malformed derived artifact references, reports their
count, and re-derives them from source text on the next write. It aborts server boot before a
migration record or schema change only when an existing artifact has no owning issue. On success
it backfills each artifact's project and generated `ref_key`.

### Pre-deploy census

Before a deployment rolls a new image, it runs `envoy-dispatch census` with the service's
`DATABASE_URL`, as a one-off task under the service's own database credentials:

```bash
DATABASE_URL=postgres://... envoy-dispatch census
```

The census is one `repeatable read, read only` transaction over the migrations this binary carries
that the database has not applied: every migration whose version `schema_migrations` does not
record, the rule the runner applies them by (a database without that table records none, so
everything is pending). For each it prints the tables the migration locks above ACCESS SHARE
(`pgmigrate.CensusTables`), with each table's total size, its row count and the sessions holding
locks on it; the transactions open longer than a minute anywhere in the database, and those whose
age it cannot see (another role's without `pg_read_all_stats`, or any with `track_activities`
off), which it tells from a session in no transaction by the virtual transaction id every
transaction locks; and the count the migration's own census answers. Those tables are:

- the ones its statements name (`pgmigrate.TouchedTables`: the targets of `alter table`,
  `create index … on`, `drop table`, `truncate`, `create trigger … on`, `update` with or without an
  alias, `delete from`, `insert into`, `lock table`, and a foreign key's `references`), read as
  Postgres's lexer reads the migration: not in its comments, its string literals or the body of a
  function it defines, and in a `DO` block's body, which it runs;
- the table behind each index it drops or alters;
- every table a foreign key reaches from a table whose rows it writes, read from `pg_constraint`
  when the census is taken, and from the `references` an earlier pending migration adds, which
  applies first at boot (in a `create table` or `alter table`, inside a `DO` block too, behind an
  `if not exists` over `pg_constraint` or not): a row inserted or updated (an upsert's `do update`
  included) is checked against the table its key references, and a row deleted, or one whose key
  an update changes, is checked against every table that references it or cascades into it, and on
  from there. The report prints such a table as `touches <table> through a foreign key with
  <table>`, and checks its holders and that it can be read, but neither counts its rows nor holds
  it to the size limit: the migration locks it only to check or act on the rows that key connects.

A table a statement only reads (`insert … select from`, `create view … as`) is not listed: its
ACCESS SHARE waits only behind an ACCESS EXCLUSIVE holder. Each store's tests apply every one of
its migrations in turn and require that reading to be the tables the migration locks above ACCESS
SHARE (`pgmigratetest.CheckTouchedTablesAgainstLocks`), so a migration written in a form the
patterns do not know (`reindex`, `cluster`, `create policy … on`, `merge into`) fails there and
needs a pattern. They apply the real set to an empty database, where a foreign key locks nothing,
since it locks only the rows it checks or cascades into; the audit's own tests seed rows to hold
the foreign-key reading to the locks it predicts. A table only a `DO` block's body names is not
held to the other half, a table read that the migration never locks: the block can branch on rows
the empty database does not hold (`if exists (select 1 from things) then update others …`). The
census checks such a table all the same. A fresh database, one that records no version and holds
no table, has no row a migration could refuse and no session to wait on: the census prints
`census: fresh database, nothing to check` and passes, so a new stack's first deploy takes the
census like every later one. It refuses:

- a migration whose census answers non-zero, naming the migration, the count and the census file;
- a table a migration names above 1 GiB (`pgmigrate.CensusTableLimit`, table, indexes and TOAST),
  whose rows it then does not count: a migration that locks a table that size needs another shape
  (`CONCURRENTLY`, batches);
- a session holding a lock on a touched table whose transaction has been open at least a minute
  (`pgmigrate.CensusLongTransaction`): the migration would give up on that lock during the rollout;
- a session holding a lock on a touched table whose transaction's age the census cannot see.
  `pg_stat_activity` shows another role's `xact_start` and `state` only to a superuser or a member
  of `pg_read_all_stats`, so without that grant every other role's holder (an operator's session)
  refuses, since the census cannot tell it is young; granting the census's role
  `pg_read_all_stats` lets it read the age, and then only a holder older than a minute refuses.
  A session with `track_activities` off shows its state as `disabled` and no `xact_start` to every
  role, so it refuses too, and the refusal says so;
- an autovacuum worker holding a lock on a touched table, only when Postgres will not cancel it for
  the migration. Postgres cancels an autovacuum that holds a lock another session waits for once
  that session has waited `deadlock_timeout`, so an autovacuum passes at any age, with the grant or
  without (to a role without it, autovacuum is the holder that runs as no role), except an
  anti-wraparound vacuum, which Postgres does not cancel, or one on a server whose
  `deadlock_timeout` is not shorter than the migration's five-second lock timeout. Postgres marks a
  vacuum anti-wraparound when it launches it, in its activity, which only a role with
  `pg_read_all_stats` can read, and which Postgres records only with `track_activities` on; where
  the census cannot read it, it refuses every vacuum of a table past its freeze age
  (`age(relfrozenxid)` or the multixact age at its `autovacuum_freeze_max_age`), one launched before
  the table passed it included;
- a touched table the census cannot read within five seconds (`pgmigrate.LockTimeout`), because
  another session holds a lock every read waits behind, or within the statement timeout (a minute,
  `pgmigrate.CensusStatementTimeout`), naming the holders;
- a census that fails or answers anything but one integer in one row;
- a database that records no version yet holds tables, on its first migration: the runner would
  apply every migration from `0001` over them.

A migration declares its census in `<version>_<name>.census.sql` beside it: one `select` (or
`with … select`) answering one integer, how many existing rows the migration would refuse (a
validating constraint's violators) or rewrite. Every migration from `censusRequiredFrom`
(`internal/dispatch/store/store_test.go`) on declares one; one that cannot refuse or rewrite a row
says so with `select 0` and a comment (`0054_message_delivery_acceptance.census.sql`;
`0053_asks_approval_kind_check.census.sql`, the check's own predicate negated, is the other
shape). A census is written against the schema just before its migration, and the store's tests
run every shipped census at exactly that schema and require it to answer
(`TestEveryShippedCensusAnswersAtTheSchemaBeforeItsMigration`). A deployment takes every census
before any migration of its release applies, so a census names only what exists before its
release. One that names a table or column the database does not have refuses, even behind an
earlier pending migration that may be what creates it: the census cannot tell that from a typo
without applying that migration, which takes the very locks it measures. A release whose census
reads what an earlier migration of the same release creates, renames or gives a new type therefore
refuses every deploy, until a release carrying the earlier migration without the later one deploys
first. The store's tests find such a pair before it ships
(`TestEveryShippedCensusNamesTheNewMigrationsItReads`,
`pgmigratetest.CheckCensusNamesTheMigrationsItReads`): a census that reads a table, column,
function or type a migration from `censusRequiredFrom` on creates, renames or, for a column, gives
a new type names that migration in a comment saying it ships in an earlier release. A rename or a
new type keeps the catalog row, so the check compares every object's name, and every column's
type, before and after each migration. Where releases split is not in the repository, so the
comment is the author's word; when both would ship together, split the release, or write the
census without what the earlier one makes, which for a new table or column holds no row at deploy
(`select 0` with a comment). The runner never runs a census. Exit 0 passes, 1 refuses (every reason
is in the report), 2 the census could not be taken (no `DATABASE_URL`, no connection, a
`search_path` naming no schema that exists, where `current_schema()` is null and the runner can
create nothing, a migration set `pgmigrate.Load` refuses).

**A census is production-executed code, reviewed like the migration beside it.** It runs as the
service's own database role. The read-only transaction stops every write, and the census's
statement is sent through the extended protocol whatever the connection string asks for, so it
cannot carry a second statement (a `commit` of its own, then a write). Read-only does not stop
`pg_terminate_backend` (which, as that role, ends the service's own sessions), `pg_cancel_backend`,
advisory locks (`pg_try_advisory_*` included) or `pg_sleep`, nor a function that runs a query
given as text (`query_to_xml` and its kin, `ts_stat`, `ts_rewrite`): `pgmigrate.Load` refuses a
census naming any of them, bare or quoted and in any case, anywhere outside its comments and
string literals, and refuses a Unicode escape (`U&"…"`, `U&'…'`) outright, since one can spell any
name and no census needs it. `Load` finds the comments and literals as Postgres 16's lexer does
(nested comments, `E'…'` escapes, continued and dollar-quoted literals), and the census runs with
`standard_conforming_strings` on, so Postgres reads its literals the same way. Review is the
control past that, since any non-zero integer a census answers is printed.

**What the report prints:** counts, sizes (in KiB, MiB and GiB, or in bytes where a size above the
limit would round to the limit's figure), versions, file names and session metadata (pid, role,
application name, state, transaction age as Postgres measured it, to the microsecond, the figure
the minute's bound is compared with; never query text, "not visible" for a field Postgres hides,
and "autovacuum worker" for one). For a census that fails, its file name and the SQLSTATE;
Postgres's own message only when the error points into the census's own text (a position, which a
parse or analysis error carries), since then it quotes that text and its identifiers. An error
raised while the census runs carries none, and its message is never printed: a data exception's
quotes the row value that failed to cast, and a `reg*` cast's (`regclass`, `regtype`, `regproc`)
quotes the row's text under a class-42 SQLSTATE; the advice about an earlier migration creating a
missing name is given only for a name the census's own text spells. The census writes nothing. An
argument `envoy-dispatch` does not know is refused with exit 2, never served, so an image that
predates a subcommand cannot boot and migrate when a deployment asks it for one.

## Reference graph

Mentions (`dispatch://` references and same-origin dashboard URLs) in document versions, ask
questions, comment bodies, and issue messages are indexed into `refs` on every write; migration
`0032_refs_provenance` gives each edge a `kind`, `created_at`, and `source_seq` (the `events.id`
that introduced it), and creates the `graph_edges` view, which unions those mentions with the
structural relations already stored on the node tables (`child_of`, `attached_to`, `anchored_to`,
`owned_by`, `replies_to`, `followed_by`). `GET /api/v1/references?to=<ref>` lists everything that
points at a node and `?from=<ref>` everything it points to, cross-project. The migration refuses
to run when an ask or comment anchor carries a non-uuid `artifact_id`.

```bash
DATABASE_URL=postgres://... envoy-dispatch rebuild-refs
```

reparses every source and reconciles the index with the text (surviving edges keep their
provenance, edges of deleted sources are removed) and prints
`rebuild-refs: documents=N asks=N comments=N messages=N orphans=N edges=N`. It reads
`dispatch.server_url` from the envoy config the same way the server does and refuses an empty
value: dashboard-URL mentions are recognised only against it, so an empty URL would drop them all.

## Search

Migration 0010 added stored generated `search` columns. Migrations `0057`–`0061` made them
plain columns that a `BEFORE INSERT OR UPDATE` trigger per table fills
(`issues_search`, `artifact_versions_search`, `comments_search`, `asks_search`,
`messages_search`), so no application code writes or refreshes a search vector. Every indexed
text, headline, and duplicate-title comparison first passes through `search_text` (0056), which
puts a space after every sixteenth underscore-joined segment. A query is not normalised and is
limited to 1,000 characters. Search covers issue titles, the latest settled document text,
comments, asks (questions and free-text answers), and messages. Live document text takes up to
the 2 s settle delay to appear in search results.

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

`GET /api/v1` (no credential) is the authoritative list: every mounted `/api/v1` route with its
`method`, `path`, `auth` (`public`, `any`, `human`, or `bearer`), and `description`, sorted by path
then method. `envoy-dispatch routes` prints the same body from the table alone, with no database
or listener and never a test hook; the docs site's HTTP API reference is generated from it. The
table below is a summary. An unknown path under `/api`, `/v1`, `/auth`, `/ws`,
or `/healthz` is a JSON 404 `{"code":"NOT_FOUND","error":"no route for GET
/v1/issues","hint":"GET /api/v1 lists every route"}`, never the dashboard shell; a missing file
under `/assets` stays `404 {"error":"not found"}`.

| Path | Method | Identity | Purpose |
| --- | --- | --- | --- |
| `/api/v1` | GET | none | List every `/api/v1` route with its auth and purpose. |
| `/auth/start` | GET | none | Start the GitHub OAuth web flow. |
| `/auth/callback` | GET | OAuth state | Exchange OAuth code, enforce allowlist, persist tokens, issue cookie. |
| `/auth/logout` | POST | cookie or trusted header | Remove the caller's stored tokens and clear the session cookie. |
| `/auth/whoami` | GET | cookie or trusted header | Return the resolved GitHub login. |
| `/auth/_dev/signin` | GET | none; `DISPATCH_DEV_SIGNIN=1` only; loopback peer, no forwarding header | Sign an allowlisted `login` in without GitHub: `302` to the sanitized `next` (default `/`) with the session cookie. `400 DEV_SIGNIN_INPUT` without `login`, `403 LOGIN_NOT_ALLOWED` off the allowlist, `403 DEV_SIGNIN_FORBIDDEN` for a non-loopback peer or a request carrying `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP` or `Via`. Each mint logs at WARN with the login and the peer. Not mounted otherwise. |
| `/api/github/rest/...` | any | cookie or trusted header | Proxy a GitHub REST request using the caller's stored token. |
| `/api/github/graphql` | POST | cookie or trusted header | Proxy GitHub GraphQL using the caller's stored token. |
| `/healthz` | GET | none | Report that the process serves, Postgres answers within `store.healthProbeTimeout` (two seconds) on the health pool — a dedicated one-connection pool, never the shared one — and NATS is connected where configured. A database that stops answering is `503` with `db: false` inside that bound, never silence, and the reason is logged. Two seconds fits the tightest prober here, the three-second compose healthcheck and deploy script, as well as the ALB's five. The body also names what is deployed: `commit`, the legion commit the image build stamped (the Dockerfile's `LEGION_COMMIT`; `null` in an unstamped build), and `schema_version`, the highest migration `schema_migrations` records, read by the same probe (`null` when `db` is false). |
| `/api/v1/events` | GET | cookie, trusted header, or bearer | Stream durable events with SSE. Omitting `since` (a cold client) subscribes before resolving the current head internally, so no separate request can race it. |
| `/api/v1/artifacts/_test/quiesce` | POST | as above, plus `DISPATCH_TEST_HOOKS=1` | Close every live document and wait for the settlements in flight; not mounted unless `DISPATCH_TEST_HOOKS=1`. |
| `/api/v1/events/_test/disconnect` | POST | as above, plus `DISPATCH_TEST_HOOKS=1` | Close every open SSE connection; not mounted unless `DISPATCH_TEST_HOOKS=1`. |
| `/api/v1/inbox?project=&assignee=` | GET | cookie or trusted header (human only) | List open asks newest-first, including their issue key, title, and assignee. `assignee=me\|unassigned\|<login>` keeps asks on issues held by the caller, by nobody (project-document asks included), or by that login; an unlisted login is `400 ASSIGNEE_NOT_ALLOWED`. |
| `/api/v1/agents` | GET | cookie, trusted header, or bearer | List live Envoy sessions (`session_id`, `title`, `dir`, `machine_id`, `roles`, `capabilities`, `last_seen`), newest first; `503 ENVOY_UNAVAILABLE` when the listener cannot be reached. |
| `/api/v1/projects` | GET | cookie, trusted header, or bearer | List projects (`key`, `name`, `open_asks`, `created_at`), ordered by key. |
| `/api/v1/projects` | POST | cookie or trusted header | Create a project from `key` and `name`; rejects a duplicate key with `409 PROJECT_EXISTS`. |
| `/api/v1/settings/repo-projects` | GET | cookie or trusted header | List external repository-to-project mappings. |
| `/api/v1/settings/repo-projects/{owner}/{repo}` | PUT, DELETE | cookie or trusted header | Create or replace, or remove, an external repository mapping. |
| `/api/v1/me/agent-tokens` | GET, POST | cookie or trusted header (human only) | List personal-token metadata or mint a personal agent token. |
| `/api/v1/me/agent-tokens/{id}` | DELETE | cookie or trusted header (human only) | Revoke a personal agent token. |
| `/api/v1/users` | GET | cookie or trusted header (human only) | The sign-in allowlist as `{users: [{login}]}`, sorted lowercase — the assignee picker's options. |
| `/api/v1/whoami` | GET | cookie, trusted header, or bearer | Who the server takes the caller for: `{kind: "user", login}` for a human, `{kind: "agent", owner, service}` for a bearer (`owner` is a personal token's lowercase login, null for the shared token; `service` is a verified service-account token's Kubernetes subject, null for every other bearer). |
| `/api/v1/issues?project=&status=&parent=&priority=&updated_since=&route_status=&limit=&offset=` | GET | cookie, trusted header, or bearer | List issue summaries: every matching issue as an array, or, with `limit` (1–250) or `offset` (0 or more; alone it pages 50), one page `{issues, total, limit, offset}` cut after every filter, `total` counting the issues they match. A repeated, blank, non-integer or out-of-range `limit` or `offset`, or any `cursor`, is `400 INVALID_QUERY` naming the parameter. The order is status, rank, creation time and key, so consecutive offsets cover the listing once while it does not change between reads; an issue that enters or leaves what the filters match, or whose status or rank changes, between two reads shifts rows across a page boundary, so one issue is served twice and another never. Only the unpaged array is an exact set in one read. Filters are optional; `updated_since` is RFC3339 and inclusive, matching issue changes and later issue events. `priority` repeats (`priority=0&priority=1`), each value `0`–`3` or `none` for an issue with no priority; any other value is `400 INVALID_PRIORITY`. `route_status` (`live`, `no_holder` or `unknown`; anything else is `400 INVALID_ROUTE_STATUS`) keeps the open issues whose route is in that state, whatever their priority; `live` or `no_holder` is `503 ENVOY_UNAVAILABLE` when the listener does not answer. Summaries contain `key`, `title`, `status`, `priority`, `parent`, `assignee`, `route`, `route_status`, `route_holder`, `updated_at`, `last_seq`, and `open_asks`. Every issue read (this list, `?pinned=true`, and `GET /api/v1/issues/{key}`) resolves `route_status` from one listener `GET /v1/sessions` per request, stored nowhere: `live` (a live session holds the role, or the session is live; `route_holder` names it), `no_holder` (nobody live holds the role, or the session is not live), `unknown` (the listener did not answer), or null with no route. |
| `/api/v1/search?q=&project=&limit=` | GET | cookie, trusted header, or bearer | Full-text search over issue titles, latest document text, comments, asks, and messages; ranked results with `<mark>` snippets and SPA `href`s; `limit` 1–50 (default 20). An under-two-character query returns `400 INVALID_QUERY`; a stop-word-only query returns `200` with no results; `400 CAP_EXCEEDED` over 1,000 characters (`contracts.SearchQueryMax`, UTF-16 units after trimming), since the query rides in the URL; `400 INVALID_PROJECT` for a project that is not a project key (none searches every project); `400 INVALID_LIMIT`. |
| `/api/v1/issues/{key}/references` | GET | cookie, trusted header, or bearer | Read the issue's eight-hop artifact reference closure. An `If-None-Match` value equal to the response ETag returns `304`. |
| `/api/v1/references?to=\|from=&kind=&since=` | GET | cookie, trusted header, or bearer | Edges of one node in the reference graph, newest first and cross-project: exactly one of `to` (backlinks) or `from` (links), each a `dispatch://` reference; `kind` filters a csv of edge kinds; `since=<events.id>` keeps mentions introduced after it (structural edges excluded). Each edge carries the other `node`, an `excerpt` (the containing block for a document mention), `created_at`, and `source_seq`. `400 INVALID_REFERENCE` / `INVALID_KIND` / `INVALID_SINCE`; `404` for a node that does not exist. |
| `/api/v1/issues` | POST | cookie, trusted header, or bearer | Create an issue and its primary document. Omitting or leaving `spec` blank gives an empty primary document at version 1. Refuses a title that near-duplicates an issue in the project with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true; external references skip the check. A spec whose ask block breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`. |
| `/api/v1/issues/{key}/asks` | POST | cookie, trusted header, or bearer | Create an optionally anchored ask. An anchor is exactly `{artifact, quote, occurrence?}` for a server-written quote mark or `{artifact, mark_id}` for a mark already written by a browser. |
| `/api/v1/issues/{key}/asks?state=` | GET | cookie, trusted header, or bearer | List an issue's asks, open and/or answered (`state`: `all` default, `open`, or `answered`). |
| `/api/v1/asks/{id}` | GET | cookie, trusted header, or bearer | Read an ask and its reply thread. |
| `/api/v1/asks/{id}` | PATCH | cookie, trusted header, or bearer | Edit one or more of `question`, `options`, `multiple`, or `urgency` on an open ask. A bearer caller must be the asking session; a human may edit any open ask. The response records `edited_at` and emits `ask.edited` with the current ask, prior mutable fields, and `edited_by`. Anchors are selected when the ask is created and cannot be changed by this route. On a block ask the `:::ask` block is written in the same transaction and the row takes the block's parsed values, so the edit versions the document once and no settlement reverts it. Only the named fields are written: `urgency`/`multiple` alone go through the attribute path and leave the body's nodes, marks and inner block ids untouched, so an untouched question keeps its formatting, links and comment anchors; naming `question` or `options` replaces that part with the markdown pipeline's own parse, and anchors inside the replaced text move as for any document edit. A field named but unchanged is not rewritten, so an idempotent retry of the whole ask writes nothing, versions nothing, keeps every anchor and returns 200. Text the block cannot carry back unchanged — an option label containing `": "`, the separator between a label and its description, is one example — is `400 ASK_BLOCK_TEXT` naming the field, with nothing written. |
| `/api/v1/asks/{id}/answer` | POST | cookie or trusted header | Answer an open ask. Every new version retracts the approval asks naming an older one, so only one an older server left open can name an older version than the document's latest settled one; answering it is `409 APPROVAL_ASK_STALE`, since the review would pin a version its question never named; review that version from the document header instead, which retracts the old ask. |
| `/api/v1/asks/{id}/resolve` | POST | cookie, trusted header, or bearer | Retract or self-resolve an open ask with a recorded reason. On a block ask the block's `state` is written with it. A reason beginning `removed from the document in version`, which marks a retraction settlement wrote and would have the retract undone when the block returns, is `400 INVALID_RESOLUTION`. |
| `/api/v1/issues/{key}/comments?artifact=` | GET | cookie, trusted header, or bearer | List comments, optionally limited to an artifact ID. |
| `/api/v1/issues/{key}/comments` | POST | cookie, trusted header, or bearer | Create a comment, root-level reply, or suggestion. An anchored comment uses the same quote-or-mark-ID shape as an ask; replies inherit their root's anchor and send none. |
| `/api/v1/comments/{id}` | GET | cookie, trusted header, or bearer | Read a comment and its reply chain. |
| `/api/v1/comments/{id}` | PATCH | cookie or trusted header | Edit a comment body. Human authors only. |
| `/api/v1/comments/{id}/resolve` | POST | cookie, trusted header, or bearer | Resolve a comment. |
| `/api/v1/comments/{id}/reopen` | POST | cookie or trusted header | Reopen a resolved thread-root comment. |
| `/api/v1/comments/{id}/accept` | POST | cookie or trusted header | Apply and accept an anchored suggestion. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and leaves the suggestion open; one removed after it answers `200` with `lost: true`. |
| `/api/v1/comments/{id}/reject` | POST | cookie or trusted header | Reject a suggestion. |
| `/api/v1/issues/{key}/messages` | POST | cookie, trusted header, or bearer | Post an issue message. |
| `/api/v1/issues/{key}/artifacts` | GET, POST | cookie, trusted header, or bearer | List issue artifacts or create a version from a multipart `file` or JSON `{name, content, summary?, actor?}`. The JSON form requires `Content-Type: application/json`. An ask block whose body breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes. A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose formatting is more items than one document update can store (1,048,576), naming the count. |
| `/api/v1/projects/{key}/artifacts` | GET, POST | cookie, trusted header, or bearer | List non-primary project artifacts (or only unlinked documents with `?unlinked=true`), or create an unlinked project artifact. An ask block whose body breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes. A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose formatting is more items than one document update can store (1,048,576), naming the count. |
| `/api/v1/artifacts/{id}` | GET | cookie, trusted header, or bearer | Read an artifact, its versions, and incoming references. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/text` | GET | cookie, trusted header, or bearer | Read a live document's markdown. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/versions/{n}` | GET | cookie, trusted header, or bearer | Read a document version or download a blob. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/versions` | POST | cookie, trusted header, or bearer | Create a named live-document version. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/edits` | POST | cookie, trusted header, or bearer | Apply document edit operations. `{id}` must be a UUID. An edit is `400 INVALID_ASK_BLOCK` when an ask it writes or changes breaks its content rule (`paragraph+ bullet_list?`) or holds what settlement cannot read; an ask it carries through unchanged is not its to refuse. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/artifacts/{id}/approval-requests` | POST | cookie, trusted header, or bearer | Ask a human to approve a document's latest settled version. The question is `Approve <name> (version <N>)?` followed by the optional `summary` (blank is `400 SUMMARY_INPUT`; past the ask cap is `400 CAP_EXCEEDED` naming `summary`). A repeat returns the ask open at that version unchanged. Every write of a new version retracts the open ask, naming the new version, in the name of the version's writer when the version credits exactly one and as `document-settlement` when it credits several or none; a request that finds one an older server left open retracts it in the requester's name and opens one at the latest version. |
| `/api/v1/artifacts/{id}/asks?state=` | GET, POST | cookie, trusted header, or bearer | List or create asks on an unlinked document. |
| `/api/v1/artifacts/{id}/comments` | GET, POST | cookie, trusted header, or bearer | List or create comments and suggestions on an unlinked document. |
| `/api/v1/artifacts/{id}/events` | GET | cookie, trusted header, or bearer | Read an unlinked document's events. |
| `/api/v1/artifacts/{id}/references` | GET | cookie, trusted header, or bearer | Read an artifact's outgoing and incoming reference edges. |
| `/api/v1/issues/{key}/artifacts/{slug}` | GET | cookie, trusted header, or bearer | Read an issue artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/issues/{key}/artifacts/{slug}/text` | GET | cookie, trusted header, or bearer | Read an issue artifact's live document markdown. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions/{n}` | GET | cookie, trusted header, or bearer | Read an issue artifact version or download its blob. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions` | POST | cookie, trusted header, or bearer | Create a named issue-document version. |
| `/api/v1/issues/{key}/artifacts/{slug}/edits` | POST | cookie, trusted header, or bearer | Apply issue-document edit operations. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/projects/{key}/artifacts/{slug}` | GET | cookie, trusted header, or bearer | Read an unlinked project artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/projects/{key}/artifacts/{slug}/text` | GET | cookie, trusted header, or bearer | Read an unlinked project document's live markdown. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions/{n}` | GET | cookie, trusted header, or bearer | Read an unlinked project document version or download its blob. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions` | POST | cookie, trusted header, or bearer | Create a named project-document version. |
| `/api/v1/projects/{key}/artifacts/{slug}/edits` | POST | cookie, trusted header, or bearer | Apply project-document edit operations. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/...` | GET | none | Serve the dashboard static files. |

## Dispatch topics

Document events publish retained envelopes on
`notifications.dispatch.document.<PROJECT>.<slug>.<type>`. Use
`go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1`
to print one matching envelope from the `natsUrls` configured in `envoy.json`.
`natstail` owns nothing on the bus - it neither creates nor updates
`ENVOY_NOTIFICATIONS` - and refuses a NATS server that is not this machine's:
prefix the command with `ENVOY_ALLOW_REMOTE_NATS=1` where `envoy.json` names a
shared server, as an agent devbox's does.

A caller resolved by header identity without a stored GitHub token receives
`503` with code `GITHUB_TOKEN_UNAVAILABLE` from GitHub proxy routes.

## Document edit operations

`POST /api/v1/artifacts/{id}/edits` accepts an `ops` array. Quote targets resolve against the
document's plain text rather than Markdown source: inline-code and link text match without their
syntax, and a table-cell target is its cell text. A quote holding a table with a row shorter than
its delimiter row, the header included, is matched as the text it is.

| Operation | Required fields | Behavior |
| --- | --- | --- |
| `replace` | `find`, `with` | Replace one plain-text target. |
| `delete` | `find` | Delete one plain-text target. |
| `insert` | `markdown`, exactly one of `after` or `before` | Insert a sibling block before or after a quote or heading's enclosing document block. `"start"` and `"end"` select document edges. Pipe-table row fragments at a table-cell target are the exception: they extend that table before or after the containing row. Use `replace` for inline continuation. |

Table-row fragments contain body rows only: omit the table header and delimiter row. Short rows are
padded to the table width while all operations in the edit request add at most 10,000 cells; a
larger request is rejected as `INVALID_OP` on `markdown`. A row holding text in a cell past the
table's width is rejected as `TABLE_WIDTH`; blank cells there are dropped.

## Document errors

| Status / code | Meaning |
| --- | --- |
| `404 TARGET_NOT_FOUND` | The quote requested by an anchor or document edit is absent, or the block id a block read (`GET /api/v1/artifacts/{id}/blocks/{block_id}`) names is not in the live document. |
| `409 TARGET_AMBIGUOUS` | A quote matches more than once without an `occurrence`; the response includes candidate ranges and context. |
| `400 TARGET_SPANS_BLOCKS` | A document edit quote crosses textblock boundaries. |
| `400 TABLE_WIDTH` | A table-row fragment holds text in a cell past its target table's width; blank cells there are dropped. |
| `409 ANCHOR_MISSING` | A browser submitted a `mark_id` that the server did not observe in the live tree. |
| `409 ANCHOR_ORPHANED` | An operation needs a mark whose anchored text has been deleted. |
| `400 INVALID_ANCHOR` | An anchor must provide exactly one of a nonempty `quote` or nonempty `mark_id`, with its document artifact. |
| `400 INVALID_MARKDOWN` | Uploaded document content cannot be represented by the Proof schema, such as a table row holding text in a cell past its delimiter row's width, which a pipe inside code or a link that is not backslash-escaped makes. Malformed edit replacements report `INVALID_OP`. |
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
  go test ./internal/dispatch/... ./cmd/dispatch/...
go vet ./...
```
