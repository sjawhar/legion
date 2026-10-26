# Dispatch HTTP server

Go binary serving the Dispatch dashboard and API. People sign in through a Cognito
sign-in pool or with Google directly and are named by lowercase email; the
web app's GitHub reads go to GitHub as the GitHub App. Dispatch keeps application
state in Postgres.

## Required configuration

Every Dispatch setting the server and its subcommands read is a row of one table,
`cmd/dispatch/settings.go`: `main` reads each row once, every reader takes its value from that
read, and a test fails on any other environment read in `cmd/dispatch` or `internal/dispatch`.
`envoy-dispatch settings` prints the table (name, `_FILE` form, default, whether it is required,
and a one-line description) without a database or a listener, and the docs site's Dispatch
configuration reference is generated from it: those are where every setting is listed. The rows
below add only what a setting's one line cannot say.

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Postgres connection string. Dispatch applies embedded migrations before serving. The pool size is fixed in code (`store.sharedPoolSize`), so a connection string carrying `pool_max_conns` is refused at startup; remove the parameter. |
| `DISPATCH_AGENT_TOKEN` | One value, or several separated by whitespace, so the shared token can rotate with an overlap: write the new value followed by the old one, move every consumer to the new one, then write the new value alone. The first is the current value; the API and the document websocket accept every value, comparing a bearer with each in constant time. Whitespace at either end is ignored; startup refuses an empty entry (two whitespace characters in a row, a CR LF among them) or a repeated one, naming its position, never its value. A request that authenticates with a value after the first logs one WARN line, `dispatch: request authenticated with a previous shared agent token`, with `entry` (the value's position), `address` (the rightmost `X-Forwarded-For` entry, which is the load balancer's view of the caller, or the connection's own address without one), `user_agent` and `path`, at most once per address and User-Agent every 10 minutes, and never a token. Each field keeps its first 256 bytes, and the server remembers up to 10,000 callers; one past that is logged on each request. Callers that share an address and User-Agent share a line, so the log says whether a value on its way out is still in use, not who uses it. |
| `DISPATCH_SIGNING_KEY` | The HMAC key every session cookie is signed and verified with, and the key the stored refresh tokens' seal is derived from (Identity, below). Unset, each process mints a key of its own in its own data directory (`auth.LoadOrCreateSigningKey`), so a deployment of more than one process sets one value every process is given: with a key apiece, a session minted on one process is refused by the other — every person is signed out by whichever process answers next — and every stored refresh token reads as absent. No process can see another's key, so nothing refuses the boot and `/healthz` cannot report it; it is a precondition of running more than one process, never a check (Running more than one process, below). Rotating it signs everyone out and leaves every stored refresh token unopenable. |
| `NATS_NKEY_SEED_FILE`, `NATS_NKEY_SEED` | The NATS nkey user Dispatch connects as: a file holding the seed (trimmed; wins), or the seed. A set but unusable value refuses startup naming the variable and path; neither set connects without a credential. |
| `DISPATCH_TEST_HOOKS` | Set to `1` to mount test-only routes: `POST /api/v1/live/_test/disconnect` closes every open live socket with 1012 (restart), and `POST /api/v1/live/_test/rotate` closes every one with 4001 (`max age`); `POST /api/v1/agents/{session_id}/stream/_test/publish` (human only) feeds the in-memory agent relay, with `as=drop` ending agent views as `dropped`; `POST /api/v1/artifacts/_test/quiesce` closes every live document and waits for settlements; `POST /api/v1/artifacts/{id}/_test/outside-schema` writes the crafted malformed document e2e uses; and `POST /api/v1/writes/_test/slow` `{method, path, delay_ms}` makes every keyed run of that method and exact path wait `delay_ms` once its key is claimed. Leave unset in every real deployment. |
| `DISPATCH_ASSET_STORE_BUCKET` | Optional bucket of immutable dashboard assets from earlier builds, each stored under its URL path without the leading slash (`assets/<file>`). When a local `/assets/*` file is absent, Dispatch reads the object under that key through the AWS SDK default credential chain, which takes its region and credentials from the task's environment. It reads the whole object, at most 8 MiB, within 3 seconds before it answers, so the browser's own download is not held to that time. It never reads `index.html` or a non-asset path from the bucket. An absent object stays a 404; any other store failure (an error, a short body, an object over the limit) is a 502 with no cache header, logged at ERROR with its key. An object stays in memory until its client has read it, so at most 64 MiB of retained objects are held at once: a request reserves 8 MiB before it asks the bucket, waiting within its 3 seconds for earlier requests to finish, and keeps its object's size until the response is written or the client goes away. A request that cannot reserve room in time, or whose admitted fetch then runs out of that same 3 seconds before the bucket answers, is a 503 with `Cache-Control: no-store` and `Retry-After: 1`, logged at WARN with its key — the bound expiring is never a 502, since the store was only asked too late to answer in time; a room refusal asks the bucket nothing. A client that disconnects while still queued for room logs nothing above a debug line and gets no response. The task role needs `s3:GetObject` on `<bucket>/assets/*` and `s3:ListBucket` on the bucket: without `ListBucket`, S3 answers a missing key with 403, and Dispatch returns 502 where it should return 404. Leave it unset for local-only static serving. |
| `DISPATCH_DEV_SIGNIN` | Set to `1` to mount `GET /auth/_dev/signin?login=<email>&next=<path>`, which signs any person in by the email it names, with no sign-in pool, so a browser or test harness can be signed in to a local instance. Boot refuses it unless identity is `cookie`, no `DISPATCH_SIGNIN_*` setting is set, the listen address is a loopback IP literal, the dashboard origin (`DISPATCH_SERVER_URL` or `dispatch.serverUrl`) names `127.0.0.1`, `[::1]` or `localhost`, every `DATABASE_URL` host is loopback or a unix socket, `DISPATCH_SIGNING_KEY` is unset, `ENVOY_ALLOW_REMOTE_NATS=1` is not set while NATS is on, `DISPATCH_AGENT_SECRETS_URL`, when set, names a loopback host, `ENVOY_URL` names a loopback host, and a loaded GitHub App private key comes from `DISPATCH_APP_PEM_B64` with `DISPATCH_GITHUB_API_BASE` naming a loopback host, never from the `pem` in `app.json`, where a developer keeps the real App's key: a signed-in session can have the App probe and import any repository it is installed on, and read GitHub through the App's proxy. That key must be a throwaway, as `packages/dispatch/e2e/run-server.sh` generates one, since every App call hands a signed App JWT to whatever listens at that base. While it is on every request must carry the dashboard origin as its `Host` (else `421 HOST_MISMATCH`). The session cookie is signed with a key generated for that process alone, so it is worthless on any other server; what a signed-in session writes to the database is not. It can mint a `dsp_` personal agent token, and its sign-out advances the person's session generation, and every server on the same database honours those rows. Give a dev-sign-in server a database no other server uses: the loopback check makes that likely, not certain, since a loopback address can be a tunnel to another machine's database or a database a second local server also runs on. Any value other than `1` or unset is refused. |
| `DISPATCH_FILE_STORE_BUCKET` | Files are stored under `files/sha256/<hash>`, one object however often a file is uploaded; a document's markdown never leaves Postgres. The credential chain on Fargate is the task role, which needs `s3:GetObject` and `s3:PutObject` on `files/*` and `s3:ListBucket` on the bucket: without `ListBucket`, S3 answers a missing object 403, which Dispatch reads as a store failure. The SDK must also find a region (`AWS_REGION`, or the shared config); without one the boot is refused. An upload writes its object before its row and before any lock or connection is taken, within 30 seconds, so a store that refuses answers 502 `FILE_STORE_UNAVAILABLE` and writes no version. The version route serves a row still holding bytes from the row and any other from the bucket: it opens the object within 30 seconds, sends its headers and `Content-Length` only then, and streams the body at the client's pace, checking it against the row's hash as it goes; a bucket that does not answer is 502, a row whose object the bucket does not hold is 500 `FILE_MISSING` (a restore from the bucket's versioning, not a retry), a cleared row on a server with the setting unset is 503, and a body that fails or hashes wrong mid-stream is cut off rather than ended as if whole. `/healthz` reports `files`: null when unset, else whether the bucket answered a two-second `HeadBucket`; it never decides `ok`, since a bucket outage must not take the one task, and with it documents and asks, down. `envoy-dispatch backfill-files` moves the rows uploaded before the bucket, and `--restore` is the rollback ("Moving uploaded files to the bucket", below). |

`DISPATCH_REPO_PROJECTS` optionally seeds repository-to-project settings at boot
with comma-separated `owner/repo=KEY` entries. Existing dashboard mappings take
precedence over this seed: it adds only a repository with no mapping, and announces each one it
adds with `settings.repo_project.updated`, as a dashboard write does. Dispatch resolves every external issue through the
stored mapping, then falls back to `DISPATCH_DEFAULT_PROJECT` when configured.
An unmapped external repository without a default project is rejected. An issue
created through the default also gets a `repo:owner/name` label.

`DISPATCH_SERVER_URL` IS the sign-in callback origin. It must equal the URL
humans type into their browser, and the sign-in pool's app client must list
`<DISPATCH_SERVER_URL>/auth/callback` as a callback URL.

`DISPATCH_ALLOWED_LOGINS` and `DISPATCH_APP_CLIENT_SECRET` are removed: a boot
that still sets either is refused, naming what replaced it.

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

GitHub App credentials come either from the `DISPATCH_APP_*` variables or from
`~/.local/share/dispatch/app.json`; the variables take precedence. Two of them
carry rules beyond their row:

| Variable | Purpose |
| --- | --- |
| `DISPATCH_APP_PEM_B64` | GitHub App private key, as a PEM or the base64 of a PEM: a value holding `-----BEGIN` is the PEM itself, and any other is base64-decoded. A key that does not parse stops boot with an error naming the variable, never the key. With `DISPATCH_DEV_SIGNIN=1` this is the only source a key may come from (a `pem` in `app.json` is refused), it must be a throwaway, and `DISPATCH_GITHUB_API_BASE` must name a loopback host. |
| `DISPATCH_GITHUB_API_BASE` | GitHub API origin override for App calls (tests and e2e point it at a fake); empty means `https://api.github.com`. With `DISPATCH_DEV_SIGNIN=1` and an App private key loaded, it must name `127.0.0.1`, `[::1]` or `localhost`. That checks the host, not what listens there: every App call hands a signed App JWT to whatever owns the port, so the key must be a throwaway. |

When no GitHub App credentials are configured, the server still starts, but
the GitHub proxy answers `503 GITHUB_TOKEN_UNAVAILABLE`, and saving a project's
architecture source answers `409 SOURCE_ACCESS` naming the missing key.

For a Google Cloud deployment, use `envoy-infra:gcp:Dispatch` from
`packages/envoy/infra`. It supplies the private Cloud SQL connection, the tailnet entry point,
and the pod environment this binary needs; see that component's README for its inputs, the IAM
grant it writes and the one the consumer writes.

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
envoy-dispatch redeliver-webhooks …`. It connects as the nkey user `NATS_NKEY_SEED_FILE` (or
`NATS_NKEY_SEED`) names and runs no `!` command, so it never reads `envoy.natsSeed`: a run against
a NATS that requires a user needs `NATS_NKEY_SEED_FILE`.

## Moving uploaded files to the bucket

With `DISPATCH_FILE_STORE_BUCKET` set, new uploads go to the bucket; the files uploaded before it
stay in their rows until `backfill-files` moves them, oldest first, one row at a time: it writes
the object under the row's hash (a write S3 verifies against that hash, and the store refuses
before any call when the bytes do not hash to it), reads it back and checks it, and only then
clears the row's bytes, in one statement that clears only a row still holding them. A row it
cannot move (a store failure, bytes that do not hash to the row's hash, a row with no hash) is
printed as `FAILED version <id>` and passed over, so one bad row holds up no other; the run exits 1
when any row failed, and can be run again at any time, since each row is done or untouched and the
server serves a row until its bytes are cleared. Two writing passes at once (`backfill-files` and
`--restore`, in any pairing) are refused by an advisory lock (`another backfill-files pass holds the
lock`). `--verify-only` reads back every cleared row against its hash, moves nothing, takes no lock
(it runs beside either pass, reading only what that pass has finished), and exits 1 naming each row
whose object is missing or wrong.

`--restore` is the rollback, at any point before or after the clear: it reads every cleared row's
object back, verified against the row's hash, and writes the bytes into the row again, so a server
with the setting unset, or an image from before the store existed, serves every file from its row.
The objects stay in the bucket, and a later `backfill-files` moves the rows out once more. Unsetting
the setting without a restore is not a rollback: a server without it serves a row still holding
bytes and answers `503 FILE_STORE_UNAVAILABLE` for every row written while the bucket was set, an
upload's as much as a cleared one's; and an image from before the store serves a cleared row as an
empty `200` under the file's own `ETag`, which a browser then caches. Restore first, then roll back.

```bash
envoy-dispatch backfill-files                 # move every row still holding bytes; prints each one and a total; exit 1 if any failed
envoy-dispatch backfill-files --verify-only   # read back every moved row; exit 1 naming any that fails
envoy-dispatch backfill-files --restore       # the rollback: write every moved row's bytes back from the bucket
```

It needs `DATABASE_URL` and `DISPATCH_FILE_STORE_BUCKET`, refuses a bucket it cannot `HeadBucket`
before touching a row, and reads nothing else of the server's configuration. Clearing a row's
bytes leaves dead tuples behind: the table shrinks on disk only after `VACUUM FULL` or
`pg_repack`, while the nightly dump shrinks at once; measure the dump, or vacuum first.

To run a local server against a bucket of your own, point the SDK at an S3-compatible server with
`AWS_ENDPOINT_URL_S3=http://127.0.0.1:<port>` (an IP literal: with `localhost` the SDK addresses
the bucket as a subdomain, which does not resolve), `AWS_REGION`, static `AWS_ACCESS_KEY_ID` and
`AWS_SECRET_ACCESS_KEY`, and `AWS_EC2_METADATA_DISABLED=true`. The Go tests start one
(`internal/tests3`, SeaweedFS in a container, since MinIO's image left Docker Hub) and create the
bucket with an anonymous `PUT /<bucket>`.

## Identity

`DISPATCH_IDENTITY` controls how browser requests identify a person, always by
lowercase email:

- `cookie` (the default) accepts signed `dsession` cookies, which a sign-in
  issues (OpenID Connect authorization code), through one of two issuers.
  Beside `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID` and
  `DISPATCH_SIGNIN_CLIENT_SECRET`, exactly one of `DISPATCH_SIGNIN_GROUP` and
  `DISPATCH_SIGNIN_DOMAIN` is set; both, some without the others, a domain
  under any issuer but `https://accounts.google.com`, a group under that one,
  or a domain that is not a lowercase DNS name refuse the boot, each naming
  what is wrong (`checkSignInSettings`).

  Through a shared Cognito sign-in pool (`DISPATCH_SIGNIN_GROUP`), the callback
  names the person by the email in their pool username (`<provider>_<email>`,
  for a provider the ID token's `identities` names), never by the `email`
  claim, which a person can write, and signs in only a member of
  `DISPATCH_SIGNIN_GROUP`; anyone else gets a 403 page naming them.

  With Google directly (`DISPATCH_SIGNIN_DOMAIN`, under Google's issuer and a
  Google web client whose redirect URI is `<DISPATCH_SERVER_URL>/auth/callback`),
  the authorization request asks for offline access with `prompt=consent`, so
  every exchange returns a refresh token, and names the domain as `hd`, a hint
  only. The callback signs in a person whose ID token's `hd` is the domain and
  whose `email_verified` is true (the JSON `true` or the string `"true"`; any
  other value is false), named by that `email`, lowercased; the token's `hd`
  decides, never the email's own domain. Anyone else gets a 403 page with the
  reason: not in the domain, email not verified, or no email. The code flow
  reads those claims from the verified token in domain mode alone; the shared
  verifier, which Cognito tokens, pods' tokens and the listener go through,
  reads none of them. A refusal logs
  `msg="dispatch: sign-in refused" email=<email> hd=<hd> reason=<reason>`, where
  a pool refusal logs `username=<username>`.

  Either way, Dispatch keeps the issuer's refresh token and confirms the person
  with it at least hourly: a refresh the issuer refuses or cannot answer (an
  outage included, so people are signed out as their hour lapses while it
  lasts), or one whose ID token no longer passes the rule or names another
  email, ends every session they hold. Logout ends Dispatch's session only, not
  the issuer's.

  A person is their email, so an address passed from one account to another is
  one person. In domain mode `people.sub` records the Google account (`sub`,
  never reused) that last signed in to the address: a sign-in under another
  `sub` ends every session of the address in the transaction that records the
  new one (a failed revoke records nothing, so the retry revokes again), logged
  `msg="dispatch: a sign-in under another Google account ended every session of the address"`
  at WARN with the email, and the hourly confirmation ends them when the
  refreshed token's `sub` is not the recorded one. A sign-in whose address
  another account signs in to between its record and its cookie gets
  `409 SIGNIN_SUPERSEDED` and no cookie: after reading the session generation
  the callback checks that the row still records its `sub`. Group mode neither reads nor
  writes `sub`, so there the old holder of a reassigned address keeps the
  cookies they have until their cookie expires, 30 days after it was issued,
  unless the pool user is removed from `DISPATCH_SIGNIN_GROUP` or disabled.

  The refresh token is stored sealed (`people.refresh_token`): AES-256-GCM
  under a key derived from `DISPATCH_SIGNING_KEY` with HKDF and a fixed purpose
  label, bound to the person's email, in a format whose `v1:` prefix names its
  version. A stored value that does not open (sealed under another signing key,
  moved from another person's row, or in plain text) counts as no refresh token:
  each read of it logs
  `msg="dispatch: a stored refresh token did not open; the person signs in again"`
  at WARN with the email and never the value, and writes nothing; the person's
  request answers `401 NO_IDENTITY`, and their next sign-in replaces the value.

  A refresh token stored in plain text (by the release before sealing, or by a
  task of it still serving during a roll or after a rollback) can be redeemed by
  anyone holding a copy of the row and the app client's secret until it expires,
  and forgetting it does not end it, so Dispatch retires it: at every boot, after
  migrations, it revokes each plain-text token at the pool's revocation endpoint
  (the `revocation_endpoint` the issuer's discovery document names) as the app
  client, then clears it and its confirmation while the row still holds that
  value. A sign-in that replaces a plain-text token retires it the same way
  first. A sealed value is never sent to the pool, and a redirect from the
  revocation endpoint is a failure, never followed. A token the boot cannot
  revoke stays in its row for the next boot and logs
  `msg="dispatch: the sign-in pool did not revoke a refresh token stored in plain text"`
  at ERROR with the email and the error (the pool's HTTP status, when it
  answered), never the token. A sign-in that cannot revoke the token it
  replaces goes on, since the person just signed in at the pool, and logs
  `msg="dispatch: a sign-in replaced a refresh token the pool did not revoke; sign this person out at the pool"`
  at ERROR with the email and the error: no later boot will see that token, so
  signing the person out at the pool is the only way to end it. The boot's
  retirement stops after 30 seconds, failing each token it has not revoked, and
  the start goes on; it logs
  `msg="dispatch: retired the refresh tokens stored in plain text" retired=<n> failed=<n>`.

  A database backup therefore holds no refresh token the pool would accept only
  once a boot of this release has retired every plain-text token, and then only
  for the tokens still in `people` when it ran: those stop working when the
  pool revokes them. A plain-text token `0068`'s release removed without
  revoking (a sign-in that replaced it, or a logout that cleared it) stays
  valid at the pool until it expires, in any backup that holds it (Database
  migrations, below). Rotating `DISPATCH_SIGNING_KEY` leaves Dispatch unable
  to open any stored refresh token, so each person signs in again, but it
  revokes none at the pool: whoever holds a backup and the old key can still
  redeem them until they expire, so after a key leak, sign each person out at
  the pool.
- `header:<Header-Name>` is for tests and local harnesses only, never for a
  production Dispatch deployment. It accepts a header value, lowercases it and
  records the person. The harness must set `DISPATCH_IDENTITY_HEADER_TRUSTED=1`;
  Dispatch refuses header identity when that flag is absent or when any
  `DISPATCH_SIGNIN_*` setting is present, so it cannot share a deployment with
  Google sign-in.

The people Dispatch has seen, either way, are the assignee picker's options and
the only names an issue may be assigned to.

`DISPATCH_INSECURE_COOKIE=1` omits the `Secure` attribute for local plain-HTTP
testing. Do not use it on an HTTPS deployment.

`DISPATCH_DEV_SIGNIN=1` adds a second way to get the cookie on a local
instance: `GET /auth/_dev/signin?login=<email>` records the lowercased person
and issues the session cookie an issuer sign-in does, with no issuer exchange and no
group check. The route serves only a loopback peer whose request carries no forwarding
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

It prints the `DATABASE_URL` to use for development and tests. Run again, it starts or reuses the
same `dispatch-pg` container. It refuses a `dispatch-pg` on any image but its pinned pgvector one,
whose `vector` extension the migrations need, and prints the commands that move that container's
data volume onto the pinned image. The database retains the migrated schema; tests only truncate
rows they create.


## Running locally

Run `bun run dispatch:local` at the repository root. It builds this server from the tree, runs it
in cookie identity with the dev sign-in route, on a seeded database, and prints the URL that opens
the dashboard signed in; `packages/dispatch/README.md` ("Local instance") documents it.

The default listen address is `:8766`. Set `DISPATCH_LISTEN_HOST` and
`DISPATCH_PORT` to change it; `DISPATCH_LISTEN_HOST` may be an IPv6 literal
with or without brackets (`::1` or `[::1]`). A bad `DISPATCH_PORT` refuses the
boot before anything connects, and an address the server cannot bind ends the
process with exit status 1.

## The live socket behind a proxy

A page's live updates ride one WebSocket per tab, `GET /ws/live`, over HTTP/1.1. A browser pools
WebSockets apart from its six HTTP connections per host, so a socket that stays open for its whole
age never holds one of the slots ordinary requests need
(`docs/solutions/dispatch-web/a-never-ending-http-request-from-a-browser-page-pins-chromes-six-connections-once-its-cert-verifier-updates-INFRA-258.md`).
That holds only while the front proxy the browser reaches (the load balancer or ingress) does not
advertise HTTP/2 extended CONNECT (RFC 8441, `SETTINGS_ENABLE_CONNECT_PROTOCOL`). A browser that
sees it runs the live socket as a stream of its HTTP/2 session instead, a session the browser marks
going away stays open while that stream lasts, and the INFRA-258 hang can return.
`bun scripts/e2e/lib/h2-settings.ts https://<dispatch>` checks it: it prints the HTTP/2 SETTINGS
the proxy sends as one JSON line, and exits 1 when they advertise extended CONNECT, 0 when they do
not or the proxy negotiates no HTTP/2, and 2 when it gets no answer (`scripts/e2e/README.md`). Run
it after any change to the proxy.

## Checking a deploy

After a deploy, check through the front proxy that the live socket, the keyed write path and the
two keyed sends work. Run the commands from a checkout of this repository with `bun install` run,
each against `https://<dispatch>`, the deployment's dashboard origin, and the two sends from a
browser signed in to it. `<token file>` holds an agent token (the shared one, or a personal `dsp_`
token from Settings → Agent tokens), and `<scratch issue>` is the key of an issue the checks may
leave comments on.

1. `bun scripts/e2e/lib/h2-settings.ts https://<dispatch>/` exits 0: the proxy does not advertise
   extended CONNECT (the section above).
2. `curl -s https://<dispatch>/healthz` prints a body with `"idempotency_keys":true`: this build
   judges `Idempotency-Key` (Keyed writes, below). The field is a fact of the build, so a `503`
   for a database or NATS that is down carries it too; a body without it is a Dispatch that would
   make a retried write twice, and the `dispatch` command sends no write again to it.
3. `bun scripts/e2e/lib/dispatch-keyed-comment.ts --url https://<dispatch> --bearer-file <token file> --issue <scratch issue>`
   posts one comment twice under one `Idempotency-Key`, as a page whose first answer was lost asks
   again. It passes when the repeat is Dispatch's stored answer and the issue holds the comment
   once: it prints `{"post":2,…,"replayed":"?1","status":201}`, `{"comments_with_body":1}` and
   `{"ok":true}`, and exits 0 (`scripts/e2e/README.md` lists every line and failure).
4. The same command with `--cli` has this checkout's `dispatch comment` post it, through a proxy
   on `127.0.0.1` that hands the command a gateway's `502` page in place of Dispatch's answer to
   its first POST, so the command asks again under its own key. It passes when it prints two
   `proxy` lines for that POST under one `key`, the first `"answered":502` and the second
   `"replayed":"?1"`, then `{"cli":{"code":0,…,"stdout":"Posted comment <id>"}}`,
   `{"comments_with_body":1}` and `{"ok":true}`, and exits 0.
5. A page opened while a write commits shows it (INFRA-292). Save the `Cookie` request header a
   browser signed in to the deployment sends (the developer tools' Network panel, any request to
   `https://<dispatch>`) to `<cookie file>`, and run
   `bun scripts/e2e/lib/dispatch-live-events.ts --url https://<dispatch> --cookie-file <cookie file> --from-whoami --subscribe-after 30 --until <marker> --timeout 120`.
   It reads `GET /auth/whoami` as you, prints `{"whoami":{"event_head":"<id>","login":"<you>"}}`
   and `{"subscribing_in":30}`, and only then opens `/ws/live` from that `event_head`, as a page
   resumes its first subscription. Inside those 30 seconds, comment on `<scratch issue>` with
   `<marker>` in the body: the comment commits after whoami answered and before the socket opens.
   It passes when it prints the comment's `{"event": …}` line and exits 0; it exits 1 when the 120
   seconds end first. Delete `<cookie file>` afterwards: it signs in as you.
6. `bun scripts/e2e/lib/dispatch-live-events.ts --url https://<dispatch> --bearer-file <token file> --until <marker> --timeout 4200`
   holds one `events` subscription on `GET /ws/live` for up to 70 minutes, longer than the 30 to
   60 minutes a socket lives, under the bearer the file holds. It prints one JSON line per frame:
   `{"event": …}` for each event, `{"close": <code>, "reason": "…"}` when the socket closes,
   `{"resubscribed": "<id>"}` when it opens the next socket and resumes after the newest event it
   holds, and `{"duplicate": "<id>"}` for an event it already printed. Once a
   `{"close":4001,"reason":"max age"}` line and the `resubscribed` line after it have printed,
   comment on any issue with `<marker>` in the body: the probe exits 0 when that event arrives on
   the new socket, and 1 when the 70 minutes end first (2 is a usage error).
7. A send across a reload: on the Agents page, open a live agent's row, set the browser offline
   (the developer tools' Network panel), send a message, and reload the tab, confirming the
   browser's prompt to leave. Within five minutes set the browser online and open the Agents page
   again in the same tab. The message lands: the row shows it once and the agent receives it once.
8. A broadcast across an outage: tick two live agents, set the browser offline and press Send.
   After 30 seconds the send's row reads `Still sending to 2 agents — Dispatch has not confirmed it
   yet. It keeps trying, and the broadcast is made once.` with **Stop trying**. Set the browser
   online: the page opens the broadcast, which lists each agent once with one delivery.

## Running more than one process

Two `envoy-dispatch` processes serve one Postgres and one NATS, so a release rolls instead of
taking Dispatch away from everyone while it deploys. A rolling update is supported, under two
preconditions, one of which the server cannot check for itself.

**Every process is given the same `DISPATCH_SIGNING_KEY`.** Unset, each process mints a key of its
own in its own data directory (`cmd/dispatch/main.go`, `auth.LoadOrCreateSigningKey`), and a
session is verified by HMAC under the key of the process that answers: at two processes with data
directories of their own, every person is signed out by whichever process answers next, and every
stored refresh token, sealed under a key derived from the same value (Identity, above), reads as
absent (`store/refresh_token_seal.go`). One process cannot detect another's key, so nothing
refuses the boot, no log line names it and `/healthz` cannot report it: it is a hard precondition
of a multi-process deployment rather than something the server checks.

**Every migration leaves the release before it able to read and write the schema** (Database
migrations, below): a rolling update runs the old build and the new build against one database at
once. A change that cannot be written that way is a stop-the-world migration, deployed with no
server of the earlier release running, and that is an exception named in the pull request.

**The release that moves live updates to `/ws/live` (INFRA-258) deploys with no server of the
earlier release running** (a Kubernetes Deployment's `strategy: Recreate`, never a rolling update),
because of its API rather than its schema. Its page keys every write with an `Idempotency-Key`
header (Keyed writes, below) and sends a broadcast without the body field `idempotency_key`, which
an earlier process requires, `400 BROADCAST_INPUT`, so a broadcast that reaches an earlier process
while the release rolls is refused; and an earlier process serves no `GET /ws/live`, so a page of
this release it answers gets no live updates. A tab opened before the deploy needs a reload either way: the
event feed it reads, `GET /api/v1/events`, is gone, so it shows `Live updates unavailable`, and
its broadcasts still carry the body field, which this release refuses as an unknown field,
`400 INVALID_JSON` (`packages/envoy/CHANGELOG.md`).

What the two processes share, and what each keeps to itself:

- Each event is published once. The outbox claims a batch (`for update skip locked`) before it
  publishes it, so two processes never publish one event twice. A process that dies between a
  publish and its `published_at` write republishes once the claim's minute lapses, which is
  at-least-once on the `notifications.role.*` core-NATS lanes, whose receivers dedupe by key.
- A reader's events subscription on its live socket carries the other process's events. It is
  re-sourced from Postgres through a per-process relay on `listen dispatch_events` (migration
  `0107_events_notify`), so an event committed on either process reaches every open subscription
  with no reconnect.
- The two delivery durables keep their shape. NATS admits one binder; the second process stands
  by, asking every two seconds, and takes the durable over within seconds of the holder dying.
- Six periodic maintenance loops run once per period across the processes, not once per process.
  Each pass takes a named session-level advisory lease for its length (`duty.Exclusive`), so a
  process whose tick lands inside another's pass logs a skip; and, under that lease, each pass is
  recorded in `duty_passes` (migration `0115`) with the loop's own period, so a process whose tick
  lands between another's passes — the ordinary case, since each ticker draws its phase at boot
  and a rolling update starts the second process minutes after the first — finds the period
  already served and logs the same skip. The settlement resumption, the delivery live hub, the
  event-stream relay, the stale-room evictor and the document rooms' timers stay per-process by
  design, and the embedding queue, the task progress reconciliation and the outbox's own scan
  claim their own work; `cmd/dispatch/main.go` says at the call site which loop is which and why.

**Two processes do not share a live document session.** Durable convergence is unconditional:
every edit is appended under the document's advisory lock, Yjs updates commute, and the stored
history is the union of them, so no edit is lost whichever process took it, no process writes a
settlement or an ask retraction from a document state it does not hold (`ErrRoomBehindHead`), and
a version whose room is behind the head is rendered from the stored document (`storedTree`). What
is per-process is the live room: while a room is resident in one process it does not receive the
other's updates, so presence and cursors show only the peers on that process. A write committed
elsewhere closes the room within a notification round trip
(`dispatch_doc_changed`, `docs/stale_room.go`), unless that room was evicted less than
`staleRoomEvictInterval` (two seconds) ago, in which case its notification is refused and the
room is re-examined when that window ends — the case the trailing write of an editing burst
meets, since the last notification of a burst is one of the refused ones. Either way the browser
reconnects, re-syncs and shows the current text. A reader sees a brief reconnect, never stale
text they can act on. Cross-process live co-editing of one document — two rooms with text
streaming between them — is a follow-up issue, out of scope here.

## Database migrations

Boot applies `internal/dispatch/store/migrations/*.up.sql` in version order and records each
version in `schema_migrations`, which it creates under the runner's advisory lock, so processes
booting together against an empty database migrate one after the other. Version 8 is an empty
file: that version was recorded from Go by the one-time conversion of pre-Proof `Y.Text` rooms and
offset anchors into Proof trees and mark anchors, which every deployed database has already run.

**A migration must leave the release before it able to read and write the schema.** A rolling
update runs the old build and the new build against one database at once (Running more than one
process, above), and the old build applies no compatibility shim: nothing at boot refuses a schema
newer than the binary knows, since the runner applies the versions it has not recorded and reads
no other (`internal/dispatch/store/store.go`, `migrate`). So a schema change is expand-then-contract
across two releases. The expanding migration adds, with a default or a backfill, and the release
that ships it writes both shapes; the contracting migration — one that drops, renames or narrows —
ships no earlier than the next release, once no running build reads the old shape:

- A `not null` column is added with a default — a constant default rewrites no row — and the
  release that ships it writes the column on every insert; the release after it may drop the
  default, once no build inserts a row without the column.
- A rename is two migrations in two releases: the new column added and filled (a backfill for the
  rows already stored), its release writing both columns, then the old column dropped a release
  later. One `alter table … rename column` breaks every statement the earlier release still sends.
- A narrowing — a type made smaller, a nullable column made `not null`, a check added — ships
  behind a release that writes only the values the narrower shape admits, and narrows in the
  release after it.

A migration that cannot be written that way is a stop-the-world migration: it is deployed with no
server of the earlier release running, and that is an exception named in the pull request, not a
default (`0113` and `0117` below are the two). The pre-deploy census ("Pre-deploy census" below) is unchanged by
that rule and still applies to every migration: it is taken before any migration of the release
applies, and a census that refuses fails the deploy before the service moves. The rule in brief
is `internal/dispatch/store/README.md`, beside the migrations directory rather than in it, since
the loader below refuses a set holding a file named anything else.

The runner records a migration by its version alone, so before it touches the database it reads
the whole directory (`pgmigrate.Load`) and refuses to start, applying nothing and naming every
file concerned, when two migrations share a version (it would apply the first and skip the rest as
already applied; keep the number on the file that merged to `main` first and renumber the rest),
when a file is named other than `<version>_<name>.up.sql`, `<version>_<name>.down.sql` or
`<version>_<name>.census.sql` (a `.down.sql` is a rollback script an operator runs by hand, and a
`.census.sql` is the migration's census, "Pre-deploy census" below; each needs its `.up.sql`),
when a census is not one `select` or calls a function it may not (below), when a version
is not decimal digits from 1 to 2147483647, and when a file cannot be read. The directory is
embedded with `all:`, which keeps a name beginning with `_` or `.`, so such a name is refused like
any other. An editor's swap file therefore stops a locally built `envoy-dispatch` from booting, and
fails the store's tests, until it is removed, and a binary built while it was there keeps refusing
to boot until it is rebuilt; Vim writes its swap file beside the file it edits by default. What no
directive embeds, such as a symlink, a `.git` or a hidden name Go's module paths refuse, never
reaches the runner, so only the store's tests catch it
(`pgmigrate/pgmigratetest.CheckEmbedsEveryFile` names each cause). Versions are applied by number,
not by file name, and the store's tests require every file on disk to be embedded
(`TestEveryMigrationFileIsEmbedded`) and the versions to run 1 to N with no gap
(`TestMigrationSetIsNumberedOneToN`), both reading file names alone, so take the next free number
on `main`.

Every migration's lock waits are bounded at five seconds (`pgmigrate.LockTimeout`, which
`pgmigrate.Exec` sets on each migration it applies, after the runner's advisory lock), so a
migration queued behind a long transaction fails the boot instead of holding every read and write
of its table behind its request. The failure names the migration, the lock it wanted, the sessions
it was queued behind, and a `pg_stat_activity` query for the sessions holding the lock; end the
holder or let it finish and start the server again. That query can come back empty: a prepared
transaction's locks carry no pid, so the join to `pg_stat_activity` drops them (LEGION-432). A
migration that needs another bound sets its own `SET LOCAL lock_timeout`.

Migrations `0056`–`0062` make search indexing linear. Each table has its own migration, so its
transaction holds an `ACCESS EXCLUSIVE` lock only for the `DROP EXPRESSION` and trigger setup;
`0062` re-indexes only rows holding sixteen or more underscore-joined segments under `ROW EXCLUSIVE`.
Their censuses answer `0` for 0056–0061, which neither refuse nor rewrite a row, and, for 0062,
the candidate rows whose stored vector the new expression changes.

Migration `0063_doc_settlements_pending` creates the table in which every durable document update
records the settlement it owes, which the settlement deletes when it commits. A room's load and the
server's minute-by-minute resumption arm the settlement a row names, so one a shutdown cuts short
still runs. It creates a table and touches no row; its census answers `0`.

Migrations `0064`–`0066` let an approval request follow its document's versions. `0064` backfills
`requested_version` into every approval ask from its `version`, rebuilds `asks_approval_kind_check`
to require it, and adds the nullable `asks.handed_back_reply_id` and an insert trigger that gives
an older binary's approval row the same value; it locks `asks` alone. `0065` sets
`comments.created_at`'s default to `clock_timestamp()`, so a comment is stamped when its insert
runs, after the owner row every comment insert takes, and an ask's newest reply is the one that
committed last; it locks `comments` alone. `0066` adds `handed_back_reply_id`'s foreign key to
`comments`, which takes `SHARE ROW EXCLUSIVE` on both tables, as adding any foreign key does: a
write waits behind it, a read does not. In one migration, the column's key held `asks`
`ACCESS EXCLUSIVE` while it waited for `comments`, and a comment write that went on to read `asks`
deadlocked with it. A comment write that goes on to write `asks` (accepting a suggestion, or an
edit under a comment's anchor, whose version moves the approval request) can still close a cycle
with `0066` while it waits for `comments`, and under the five-second bound either side could lose
it: Postgres checks a waiter once, `deadlock_timeout` (1 s by default) after it starts to wait, so
a write that closes the cycle within that second fails `0066` and one that closes it later is
itself the victim, answered `500`. `0066` therefore sets its own `lock_timeout` of 500 ms, shorter
than `deadlock_timeout`: it gives up first (`55P03`, nothing applied), the write finishes, and the
next boot applies it; the cost is that a comment write holding `comments` past half a second fails
that boot too, and so does an autovacuum of either table where `deadlock_timeout` is not shorter,
which the census refuses (below). Every census answers `0`: 0053's check guarantees every approval
ask a `version` to backfill from, and the other two change no row.

Migration `0067_user_agent_reply_read` creates the table of replies a human has read by id, which
the broadcast page writes for the replies it shows and the unread count leaves out, beside the
per-session read mark, and its `(login, session_id)` index, which the read mark's prune of one
session's rows reads. It creates a table and touches no row; its census answers `0`.

Migration `0068_people` creates `people`, everyone who has signed in, holding the sign-in pool's
refresh token for a person who signed in through it, and drops `users`, the GitHub OAuth token pair
of each GitHub login that signed in, which the GitHub proxy acted with before it read GitHub as the
App. It locks `users` alone, `ACCESS EXCLUSIVE` for the drop; `people` is new. Its census counts the
`users` rows the drop deletes.

No migration retires the refresh tokens that `0068`'s release stored in plain text: every boot does
(Identity, above), since a task of that release can write one during the roll, or after a
rollback, once every migration has run, and a token written after a boot waits for the next one.
So once the last task of the earlier release is gone, the deployer restarts Dispatch once and then
checks three things: that boot logged `failed=0`; no
`a sign-in replaced a refresh token the pool did not revoke` line has appeared since the roll began
(each one names a person to sign out at the pool); and
`select count(*) from people where refresh_token is not null and refresh_token not like 'v1:%'`
answers `0`. A boot line with `failed=<n>` above zero names each token it could not revoke in an
ERROR line; the next boot tries again, and signing that person out at the pool ends it at once. A
non-zero count with no such line is a token a task of the earlier release wrote after the last
boot, which another restart retires.

The boot revokes only the plain-text tokens still in `people`. Under `0068`'s release a sign-in
that replaced a person's token, or a logout that cleared it, removed it without revoking it, and
it stays valid at the pool until it expires in any backup that holds it. So if any database backup
was taken between `0068`'s deploy and the post-roll restart, either delete those backups or sign
out at the pool everyone whose `people.signed_in_at` is after `0068`'s deploy.

Migration `0071_agent_artifacts` gives an artifact a third owner, an agent's conversation: the
nullable `artifacts.session_id`, a check that every artifact has exactly one owner (an issue,
`issue_key` with its `project_key`; a project, `project_key` alone; or a session, a non-empty
`session_id` alone), and `project_key` made nullable for the session's rows. `ref_key`, 0009's
generated column, becomes a plain column that a `BEFORE INSERT OR UPDATE` trigger
(`artifacts_ref_key`) fills, as `0058` did for `search`: `DROP EXPRESSION` keeps every value and
rewrites no row, the trigger computes 0009's expression for every other row and
`agent/<session id>/<slug>` for a session's, and `artifacts_ref_key_key` keeps it unique. It adds
the partial index `artifacts_session_id`, which serves the two lookups an upload to an agent's
conversation makes by `session_id` (its artifact by name, and whether a slug is taken). It locks
`artifacts` alone, `ACCESS EXCLUSIVE` for milliseconds while the check reads its metadata rows; its
census counts the rows the check would refuse, `project_key is null`, which 0009's `not null` keeps
at `0`.

Migration `0082_answers_by_person` adds two partial indexes for `GET /api/v1/me/answers`: one on
`events` for user-authored `ask.answered` events, which the route filters to events whose actor is
the answer's user, and one on `comments` for user replies on asks. Building them takes `SHARE` on
`events` and `comments`; its census answers `0` because it refuses and rewrites no row.

Migration `0087_search_vector_bound` creates `search_vector` and replaces the bodies of four
search triggers' functions (documents, comments, asks and messages) so that each builds its vector
with it ([Search](#search)). Replacing a function locks no table, and for every text whose vector
fits, which is every row already stored, the new functions build the vector the old ones did, so
it re-indexes nothing; its census answers `0`.

Migrations `0088_issues_title_lexemes` and `0089_issues_title_lexemes_backfill` store each issue
title's lexemes, which the duplicate-title check reads ([Search](#search)). `0088` adds
`issues.title_lexemes` with the constant default `{}`, a catalog change that rewrites no row, so it
holds `issues` `ACCESS EXCLUSIVE` for milliseconds (3 ms on 2,905 issues with production's titles),
creates `title_lexemes(title)`, the one definition of a title's lexemes, and has the issues trigger
build `search` with `search_vector` and fill the column on every insert and every retitle. `0089`
fills it on every issue stored before, under `issues` `EXCLUSIVE` taken before it writes a row: a
read does not wait, while every write of an issue's row, and of a row that references an issue,
waits until it commits (0.58 s on those issues at load 85). The update alone locked each issue's
row in the order rows lie on disk, which a reparent, locking its two issues in key order, could
cross: on 60,000 issues the two deadlocked (`40P01`) in each of two runs, and with the table lock
the reparent waited for the migration and committed. Neither refuses a row; both censuses answer
`0`.

Migration `0091_artifacts_project_documents` adds the partial index
`artifacts_project_documents`, on `artifacts (project_key)` where `issue_key` and `session_id` are
null: a project's unlinked documents. Settlement of a project document holding an ask block no ask
of its own indexes reads its project's other documents through it, for the ask the block may be a
copy of and for the copies of an ask it retracts, and so do the answer and resolve routes, for the
copies of the ask they close (LEGION-651). Building it takes `SHARE` on `artifacts`; its census
answers `0` because it refuses and rewrites no row.

A decided ask that a release before LEGION-651 reworded gets back, in code, the wording its decision
was given on: the `previous` wording of the first `ask.edited` after its latest `ask.answered` or
`ask.resolved`, with no `ask.opened` between. No migration rewrites those rows, since the census
refuses one that does (Pre-deploy census, below). Every boot runs the restoration over every
decided ask in one statement (`docs.Service.RestoreDecidedWordings`, bounded at 30 s), reading
`events` through `events_ask_payload_id` and writing only the rows whose wording differs; it logs
`dispatch: restored a decided ask's wording` for each with the ask, its document and the question
it asks again. In the same transaction it marks owed, in one upsert, the settlement of each
restored block ask's own document, whose block still asks the rewording beside the decision, and
of every other document of each restored ask's owner whose latest version holds the ask's block
with no ask of its own under that id, a copy that may have matched the rewording and shown the
decision on it, and arms those settlements once it commits; then it logs `dispatch: restored
decided asks' wordings` with `restored=<n> copies=<n>`. On the 2026-10-11 production dump (3,739
asks, 82,153 events) it restores 5 asks, each a block ask on a document of its own, and marks 0
copies owed, in 0.17-0.77 s: the one other document holding one of those 5 blocks has an ask of
its own under that id, so it is no copy, and the first boot of the release that carries it logs
`restored=5 copies=0` and settles those 5 documents; each whose block still asks the rewording gets
the decided wording written back and logs `dispatch: wrote a decided ask's wording back into its
block`. Each document's settlement runs the same statement over its own decided asks before it
reconciles them, and marks the restored asks' copies owed the same way, so a rewording a task of
the earlier release writes during the roll is undone by that document's next settlement on this
release, which also writes the decided wording back into the block and settles its copies again.

Migration `0092_refs_excerpts` adds three nullable columns to `refs` with no default
(`excerpt_block_id`, `excerpt_text`, `excerpt_version`), a catalog change that rewrites no row,
and the partial index `refs_excerpt_drift` over the rows whose `excerpt_version` is null: the
stored excerpt `GET /api/v1/references` answers a document mention with, which every version write
fills and the reference-excerpt reconciliation fills for every row stored before it
(`packages/envoy/AGENTS.md`). On the 2026-10-10 production dump the reconciliation's first pass
filled 7,498 mentions of 2,150 documents in 248 s at `GOMAXPROCS=2`. Its census answers `0`.

Migration `0094_issue_links` creates `issue_links`, the issues each issue waits on through
`blocked_by`, and its target index, and re-states the `graph_edges` view with the `blocked_by` arm.
The table's two foreign keys take `SHARE ROW EXCLUSIVE` on `issues` for the migration's short
transaction, so an issue write waits behind it while an issue read does not, and re-stating the
view holds `graph_edges` `ACCESS EXCLUSIVE`, so a reference read waits too. It touches no row; its
census answers `0`.

Migrations `0095_user_threads` through `0099_user_threads_catch_up` build `user_threads`, the
table behind the Replies page (`packages/envoy/AGENTS.md`). `0095` creates the table, its partial
index `user_threads_unread` and its `(thread_id)` index, the functions `user_threads_login`,
`user_threads_record`, `user_threads_comment` and `user_threads_message`, and the trigger on
`comments`; `0096` adds the trigger on `asks` (`insert or update of answer`); `0097` attaches
`user_threads_message` to `messages`. Each trigger is its own migration, so its transaction holds
that one table's `SHARE ROW EXCLUSIVE` alone: a write to it waits behind the migration and a read
does not. `0098` seeds a row for every person and thread they wrote in, under `ACCESS SHARE` on
`comments`, `asks`, `messages`, `issues` and `artifacts`, writing only `user_threads`; every row
starts read, its read mark moved to the newest reply already stored, so the deploy turns no stored
reply into an unread one. The triggers are live by then, so a write that lands while it runs on a
thread whose participant already has a row is caught by the trigger, and the seed merges with it
by taking the later of each time. A participant the seed is still inserting has no row the
trigger's update can see, and the seed's snapshot cannot see that write, so `0099` reads the same
writes under `ACCESS SHARE` on `comments`, `asks` and `messages` and moves each row to any write by
someone else newer than its `replied_at`, which makes it unread past its read mark; it moves
nothing backwards, and a write the seed saw stays read. On a copy of the 2026-10-12 production
dump (11,497 comments, 3,882 asks, 14,289 messages) the seed wrote 3,819 rows for three logins,
all of them read, in 0.93 s, and `0099` ran in 0.11 s under the server's own runner and moved no
row, taking `ACCESS SHARE` on the three tables it reads and `ROW EXCLUSIVE` on `user_threads`
alone. All five censuses answer `0`: `0095`-`0097` create functions and triggers and refuse or
rewrite no row, and `0098` and `0099` write only the table `0095` creates.

Migration `0113_repo_projects_default_and_issue_repo` adds `repo_projects.is_default` and
`issues.repo` (`packages/envoy/AGENTS.md`). From its release on, an issue sits at most 32 links below
its root, and the API refuses a write past that (`409 PARENT_INPUT`); the release before it did not,
so `0113` refuses to apply while any issue sits more than 32 links below its root or in a parent
loop, naming up to 20 of them and the repair: `PATCH /api/v1/issues/<ancestor>` with
`{"parent": null}`, on the release still running. Its census counts the same issues. It takes
`repo_projects` and `issues` in `ACCESS EXCLUSIVE` before it counts and holds both to commit, so a
write of the earlier release that commits while it waits is in the count.

**`0113` was deployed with no server of the earlier release running, and no release older than it
is started over it again.** That instruction was `0113`'s own, not the rule for `0114` and after:
`0113` is applied in production, and a release whose migrations are written to the rule above
rolls (Running more than one process, above). Its own deploy stopped the earlier release, or
scaled it to zero, before the new release booted and migrated — for that deploy, a Kubernetes
Deployment's `strategy: Recreate`. A failed deploy recovers forward, not by rolling the image
back. Only the new release's API holds the limit; nothing in the database does. Boot has no check
that refuses a schema newer than the binary knows: the runner applies the versions it has
not recorded and reads no other (`internal/dispatch/store/store.go`, `migrate`). So an earlier
release left serving after `0113`, or started again over it by a rollback, writes as it always did,
and can build a chain past the limit again. Such a chain, or one written by direct SQL, fails every
read of its issues with `500`, a project's list included. The API repairs an acyclic one: `PATCH`
an ancestor that still reads (its `GET` answers `200`) with `{"parent": null}`. The `PATCH` reads
the issue it changes first (`api/issue_patch.go`), so a parent loop none of whose members reads,
which only direct SQL can write, needs a database repair:
`update issues set parent_key = null where key = '<a member>'`. The auto-deployer
(`packages/envoy/deploy/README.md`) keeps both rules. Compose stops and removes the old `dispatch`
container before it starts the new one. When a deploy fails, the auto-deployer stops the failed
image and confirms the stop before it reads the schema, and when the schema has moved past the one
the previous image runs against, it refuses to start that image again and exits `1` for a forward
recovery.

Migration `0117_architecture_sources_per_repo` lets a project import its components from more than
one repository (`packages/envoy/AGENTS.md`). `architecture_sources` gains `id`, which names a
source within its project, and its primary key moves from `(project_key)` to `(project_key, id)`,
with a unique index on `(project_key, repo)`, so a repository is a source of a project at most
once. `components` gains `source_id` and a foreign key on `(project_key, source_id)` to its
source's row, `on delete cascade`, so deleting a source deletes its components. Both columns are
added with the constant default `0`, which rewrites no row, and the default is then dropped:
every existing source reads `id` 0, every existing component `source_id` 0 (its project's one
existing source), and a source added later gets an `id` from 1 on. It takes `architecture_sources`
and `components` in `ACCESS EXCLUSIVE` first and holds both to commit. Its census counts the
components whose project has no source, the rows the foreign key would refuse:

```sql
select count(*) from components c
where not exists (select 1 from architecture_sources s where s.project_key = c.project_key);
```

Only direct SQL writes such a component: the importer writes components under their source's row
lock, and deleting a source deletes its components. A census that refuses on it is repaired on the
release still serving, before the deploy, by deleting them:
`delete from components c where not exists (select 1 from architecture_sources s where s.project_key = c.project_key)`;
then take the census again.

**`0117` is a stop-the-world migration: deploy it with no server of the earlier release running,
and never start a release older than `0117` over it again.** Stop the earlier release, or scale it
to zero, before the new release boots and migrates: a Kubernetes Deployment's
`strategy: Recreate`, which `envoy-infra:gcp:Dispatch` uses at its default one replica
(`packages/envoy/infra/README.md`); a deployment running more than one replica, which rolls, is
scaled to zero first. The compose auto-deployer above stops the old container before it starts the
new one. A failed deploy recovers forward, not by rolling the image back. The earlier release does
not work over the new schema, and nothing at boot stops it from serving:

- Its save of a project's source upserts `on conflict (project_key)`, which no unique index covers
  once `0117` commits, so Postgres refuses the statement and the save answers `500`.
- Its import takes one source row of the project as the project's one source, whichever row
  Postgres returns first. When the model changed, it deletes every component of the project, every
  source's, and inserts that source's components with no `source_id`, which has no default. A model
  holding a component fails on the not-null column and rolls back, and its failure record writes
  the error onto every source row of the project. A model holding none (no file in that branch's
  `.dispatch/architecture/`) commits the delete, and every source's components are gone.
- Its delete removes every source of the project and every component of the project.

Migration `0118_request_keys` creates `request_keys`, the one record of every keyed write (Keyed
writes, below): a new table that touches no row, which the release before it neither reads nor
writes; its census answers `0`. It follows `0116_project_default_assignee` and
`0117_architecture_sources_per_repo`, which already use the preceding migration numbers. From its
release on, nothing reads or writes `broadcast_idempotency_keys` (`0055`), the broadcast's own key
table before it, and the table is kept: dropping it is a contracting migration, so it ships no
earlier than the release after this one.

Migration `0119_pending_delivery_indexes` adds two partial indexes, `message_deliveries_pending` and
`comment_deliveries_pending`, on the delivery attempts still pending, which the delivery sweep reads
every minute (Keyed writes, below). It reads, refuses and rewrites no row, and the release before
it neither reads nor depends on them; its census answers `0`. Each build holds its table's writes
(a `SHARE` lock) for as long as it reads the table once.

Migration `0009_project_artifacts` deletes malformed derived artifact references, reports their
count, and re-derives them from source text on the next write. It aborts server boot before a
migration record or schema change only when an existing artifact has no owning issue. On success
it backfills each artifact's project and generated `ref_key`.

A migration runs while the build before it is still serving. The app Deployment of
`envoy-infra:gcp:Dispatch` rolls above one replica (`packages/envoy/infra/README.md`), so from the
start of a rollout until the last pod of the previous release is gone — and again for as long as a
rollback lasts — the previous image's queries run against the new schema. A migration may
therefore only add: a table, an index, a trigger, a column that is nullable or defaulted, a
constraint added `not valid`. In the same release as the code that depends on it, it may not drop
or rename a column or a table, change a column's type, or tighten a constraint the running build's
queries would violate. Each of those is split across two releases: the first makes the schema
accept both shapes, and the second removes the old one once no pod serves it.

The census below does not cover this. It reports locks, sizes and counts — what a migration would
wait behind and how much it would rewrite — not whether the previous build's queries still answer
against the new schema; that one-release gap is the author's to reason about. The five-second
`pgmigrate.LockTimeout` also bites harder under a rolling update than under `Recreate`, which
stops the previous pods before the new one boots: while a deployment rolls, the old build's
connections are still open and still holding locks on the tables the migration wants, so a
migration that applies cleanly against an idle database can fail the new pod's boot.

### Pre-deploy census

`envoy-dispatch census` is run before a new image goes out, with the service's `DATABASE_URL` and
under the service's own database credentials, and it is a gate: a census that refuses stops the
deploy before the service moves, and nothing accepts a refusal and rolls past it. No deployment of
`envoy-infra:gcp:Dispatch` runs one — the component registers no census task — so an operator runs
it by hand against the deployment's database before updating the image, with the `kubectl run`
recipe in `packages/envoy/infra/README.md`.

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
- every table a foreign key reaches from a table whose rows it writes, or from a table it drops,
  read from `pg_constraint` when the census is taken, and from the `references` an earlier pending
  migration adds, which applies first at boot (in a `create table` or `alter table`, inside a `DO`
  block too, behind an `if not exists` over `pg_constraint` or not): a row inserted or updated (an
  upsert's `do update` included) is checked against the table its key references, a dropped
  table's own foreign keys go with it and take ACCESS EXCLUSIVE on each table they reference, and a
  row deleted, or one whose key an update changes, is checked against every table that references
  it or cascades into it, and on from there. The report prints such a table as `touches <table>
  through a foreign key with <table>`, and checks its holders and that it can be read, but neither
  counts its rows nor holds it to the size limit: the migration locks it to check or act on the
  rows that key connects, or, for a drop, only for the catalog change, reading no row.

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
  `deadlock_timeout` is not shorter than a lock timeout the migration waits under: the runner's five
  seconds, or the one its own `SET LOCAL lock_timeout` sets, which Postgres reads in a savepoint of
  the census's (a value it refuses there refuses the migration). `0066` sets 500 ms, so an
  autovacuum on `asks` or `comments` refuses it at the default one second. Postgres marks a
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

So a migration that changes values readers already had, whose census counts the rows it rewrites,
cannot pass the census wherever such a row exists, and a release that must repair existing rows
does so in code instead: an idempotent pass at boot, and on the path that reads the rows, so a task
of the earlier release still running during the roll cannot undo it. The restoration of decided
asks' wordings (`docs.Service.RestoreDecidedWordings`, Database migrations above) is one.

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
points at a node and `?from=<ref>` everything it points to, cross-project. A row whose other end is
a node kind the running build does not know (a newer build's, read during a rolling deploy) is left
out of every list and every count that build serves (`nodeLoaders`, `refs/graph.go`, names the
kinds it knows). The migration refuses to run when an ask or comment anchor carries a non-uuid
`artifact_id`.

```bash
DATABASE_URL=postgres://... envoy-dispatch rebuild-refs
```

reparses every source and reconciles the index with the text (surviving edges keep their
provenance, edges of deleted sources are removed) and prints
`rebuild-refs: documents=N asks=N comments=N messages=N orphans=N edges=N`. It reads
`dispatch.server_url` from the envoy config the same way the server does and refuses an empty
value: dashboard-URL mentions are recognised only against it, so an empty URL would drop them all.

A picture in a message, comment, reply, ask question, answer or resolution reason is CommonMark's
image syntax whose address is the picture's own Dispatch reference, versioned:
`![<file name>](dispatch://<owner>/artifact/<slug>@v<N>)`, where the owner is an issue key, a
project key, or `agent/<session id>` for an agent's conversation. The reference inside is indexed
as any mention is. An agent's conversation's file is `dispatch://agent/<session id>/artifact/<slug>[@vN]`,
the session id as written, and its dashboard page `/agents/<session id>/artifacts/<slug>[?v=N]`
names the same artifact; a session id holding a `/`, `?`, `#` or whitespace names nothing.
`graph_edges` has no structural edge for such a file (no `attached_to`, since it belongs to no
issue): it appears in the graph through the mentions that cite it.

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

Every trigger builds its vector with `search_vector` (0087; the issues trigger's from 0088).
Postgres holds at most 1,048,575 bytes of lexemes and positions in one vector. Prose stays far
below that, since its words repeat, but text of words no two alike passes it well inside a
document's 1 MiB: about 700 KB of `w000001 w000002 …`, or 475 KB of UUIDs. `search_vector` indexes a
text whole when its vector fits, and otherwise the longest of its first half, quarter, eighth, …
that fits, cut between two words, so keyword search finds such a document, comment, ask or message
by the whole words in that opening part and not by the words after it; a text with no whitespace
before the cut is found by none of its own. An issue's key is always indexed whole. Meaning search,
where it is configured, ranks by an embedding of a text's first 32,000 characters
(`embed.maxInputChars`), not by its vector, so it can still return such a text for a query whose
words the vector does not hold.

Each kind of content is ranked on a list of its own, by `ts_rank_cd`, then recency, then id, and
the five lists are merged by reciprocal rank fusion: a hit at position p of its list scores
1/(60 + p), which is its `rank`. The page therefore takes each kind's best in turn, the best issue,
document, ask, comment and message first, rather than a run of long documents that repeat the
query's words. Hits that score alike are ordered issue, document, ask, comment, message, then by
recency and id, so the order is total and consecutive `offset`s walk it once while the corpus holds
still. An issue whose key is the whole query heads the issue list. Each list keeps its best
`contracts.SearchKindDepth` (100) hits: the answer's `total` counts every match of every kind, and
`reachable` the hits the pages can return, so an `offset` at or past `reachable` answers no hits
with both counts.

Search snippets are escaped text with only server-inserted `<mark>` elements around matches. Native
issue creation rejects a title that near-duplicates an existing issue in the same project with
`409 POSSIBLE_DUPLICATE` and up to five candidates; `force` bypasses that check, and external
references skip it. An issue title is at most 1,000 characters (`contracts.IssueTitleMax`, UTF-16
units after trimming), on creation and on a retitle, and a longer one is `400 CAP_EXCEEDED` before
the duplicate check runs. The check compares the new title's lexemes with those of every other
title in the project, read from `issues.title_lexemes`, which the issues trigger fills with
`title_lexemes(title)` (0088), the function the check calls for the new title, so a creation parses
one title; a candidate's snippet marks the words its title shares with the new one, and the
`POSSIBLE_DUPLICATE` message quotes a candidate's whole title. The cap bounds what one stored title
adds to every later creation in its project, and how long that message can be: beside 2,000 stored
titles at the cap, a check takes 56–95 ms, where parsing every title as it ran took 2.1–7.6 s.

`POST /api/v1/issues`, `POST /api/v1/issues/{key}/asks` and `POST /api/v1/artifacts/{id}/asks`
also return, under `advice.suggestions` and never blocking or slowing the write past
`writeSuggestionTimeout` (300ms), the three fused search hits most like what was just filed
(`related`) and, when an already-answered ask settles the same question, that decision with who
answered and when (`decision`); `missing` says why search did not answer in time instead of
listing anything (LEGION-550). The search is scoped to the write's own project, and leaves out
everything its owner holds — the new issue and its spec, or the issue or project document an ask
sits on and everything inside it — so nothing is ever suggested that the writer already has.
A hit on an issue that is `done` ranks below every hit on an open owner, since keyword search
otherwise puts an issue already closed as a duplicate above the open issue it was closed into.
Every suggestion offered is recorded in `write_suggestions`, whose `outcome` the daemon's outcome
sweep (`api.RunSuggestionOutcomeSweep`, every `SuggestionSweepInterval`) advances from `ignored`
to `acted_on` (the suggested item was cited from the source, or the suggested issue was updated
directly) or `overridden` (the source got further activity instead) — a count of how often the
suggestion was right, without refusing anything on its strength.
A ranking or latency change is measured on a restored copy of the production corpus:
`packages/envoy/scripts/corpus-copy.sh` restores one, `search-smoke.sh` prints what a searcher
sees for a list of queries from any build, and `search-latency-compare.sh` runs
`TestSearchLatencyOnCorpus` from a base checkout and this one alternately and gates on the
difference (`packages/envoy/scripts/README.md`, "Measuring search on a copy of the corpus").

## Keyed writes

Every `POST` and `PATCH` route takes an optional `Idempotency-Key` request header naming the
intent behind the write, the same on every retry of it and new for every new one (the IETF
`Idempotency-Key` draft; Google AIP-155). The dashboard mints one per press and the `dispatch`
CLI one per command's write, and both retry under it by one rule (`retriesUnderKey` in
`@legion/contracts`): a failed fetch, an attempt silent too long, a 5xx, a 408, a 429 and a
`409 KEY_IN_FLIGHT`. No route requires the header: a write without one runs as it always
did, unkeyed, so a script or the Legion daemon's outbox keeps working. `PUT` and `DELETE` take no
key, being idempotent already. A key is 1 to 128 letters, digits, `.`, `_`, `:` and `-`, bare or
double-quoted, and belongs to the caller who sent it: a person's canonical login (from the
dashboard or through a personal token), a service token's subject, or the shared agent token.
Dispatch keeps each key's answer for 24 hours (`request_keys`, migration `0118`; the
suggestion-outcome sweep deletes older rows).

A keyed write finishes on the server whatever its client does. Its handler runs with the client's
cancellation taken off, bounded at ten minutes (past which its work rolls back and the key is
released, as for a 5xx) and counted in the server's background wait, so a shutdown lets it finish
as it lets a delivery finish. While it runs it renews its key's 60 s lease every 20 s, so a
client that gave up and asks again is answered `409 KEY_IN_FLIGHT` for as long as the first run
lives, then that run's stored answer; only a run whose process died is taken over, once its lease
lapses. A write Dispatch takes longer to finish than its client waits still lands once.

| Status / code | Meaning |
| --- | --- |
| The first answer, with `Idempotency-Replayed: ?1` | A repeat under a key whose first request finished, with the same method, route, path values, query, media type and body: the stored status, every header the first answer set and its body, and nothing runs again. |
| The first status with `ANSWER_NOT_KEPT` | That repeat, when the first answer was larger than the 1 MiB a key keeps: the write ran, and the caller reads what it wrote again. |
| `409 KEY_IN_FLIGHT`, `Retry-After: 1` | A repeat while the key's first request still runs (its lease renewed every 20 s while it does); nothing ran, and the caller asks again under the same key. A repeat once the lease has lapsed, 60 s after the last renewal, takes over a key whose holder died before it committed anything, and runs the request. |
| `409 KEY_COMMITTED`, no `Retry-After` | A repeat whose first request committed a write but whose answer was lost: its lease lapsed in flight, or it ended in a 5xx or a panic after committing (every transaction a keyed request commits that wrote a row marks the key's `committed_at` in that same transaction; a lock-only `SELECT … FOR UPDATE` marks nothing, and a COMMIT refused after the mark leaves none). The write landed and is not run again; there is no answer to replay, so the caller reads what it wrote. Also a repeat of a keyed `POST /api/v1/me/agent-tokens` that minted, once that first request has finished, answer lost or not: the mint's answer carries the plaintext token, so it is never kept under the key, never replayed, and no second token is minted (a mint refused with a 4xx commits nothing, so its key is released and a repeat runs it again). That is the one route whose answer is not stored. |
| `422 KEY_REUSED` | The key was already used for a different request; the message names its method and path, and this request did not run. A changed request needs a new key. |
| `400 KEY_INVALID` | The header is not 1 to 128 of those characters, or was sent twice. |
| `409 LOGIN_CHANGED` | The write names its presser in `Dispatch-Login` (every write the page makes does, `POST`, `PATCH`, `PUT` and `DELETE` alike) and its caller is someone else, or no person at all (any bearer): nothing ran and the key was not read, so it is untouched. A caller that resolves to nobody (a lapsed cookie) gets the route's own `401` instead, and a write without the header is not checked. The page drops every write that login pressed. |
| `400 REQUEST_BODY_UNREADABLE` | The client did not finish sending the body the layer reads before the handler; nothing ran and the key was not claimed, so the same key may be sent again. |

A 5xx, a 401, a 408 and a 429 are not stored: the key is released and the next attempt runs the
request again, unless the request had already committed a write, which makes that next attempt
`409 KEY_COMMITTED` at once. Four kinds of write keep their own recovery point beside the key, so a
repeat of `POST /api/v1/agents/{session_id}/messages`, `POST /api/v1/broadcasts`,
`POST /api/v1/issues/{key}/messages`, or a comment (`POST /api/v1/issues/{key}/comments`,
`POST /api/v1/artifacts/{id}/comments`) answers what it made as it stands now, under the first's
`201`, however the first request ended, and puts on its way whatever delivery a dead process left
owed; a broadcast's repeat answers at once and sends what it owes behind the answer. A comment's
or an issue message's replay reads every member fresh and keeps from the first answer only its
`advice` and `ask_waiting_on`, and only when that answer was a 2xx. Neither send route takes a body
`idempotency_key` any more.

A delivery a dead process left owed is made whether anyone asks again or not. Every write that
delivers to sessions claims each attempt before its send and settles it after, on a context its
client's cancellation does not reach. The maintenance loop's pass, once a minute, sends each
attempt that is still pending under a claim that has lapsed, is its message's or mention's latest,
and was opened over a minute ago and inside the 72-hour duplicate window
(`DELIVERY_DUPLICATE_WINDOW_MS`), under its own attempt number, so the listener drops a frame that
did land; and the first attempt of a keyed send whose process died between committing its message
and opening that attempt. A send to one session answers once its own attempt has settled, which
keeps a person's sends to one session in the order they pressed them; a broadcast answers once its
messages are committed, with its deliveries behind the answer.

The layer reads a keyed body before its handler runs, spooling at most the route's own limit: 26
MiB for the three upload routes (`POST /api/v1/issues/{key}/artifacts`,
`/projects/{key}/artifacts`, `/agents/{session_id}/artifacts`), 1 MiB for every other. A body past
it runs unkeyed and its route refuses it. A body the client did not finish sending is
`400 REQUEST_BODY_UNREADABLE`; one Dispatch cannot spool to its temporary directory is Dispatch's
own failure, a 5xx with the key echoed, which the client asks again.

Every answer the layer judged echoes the key back (`Idempotency-Key: <key>`) - not the
`400 KEY_INVALID`, a request whose caller it could not resolve, or one whose body is
past its route's limit, each of which runs unkeyed - and `/healthz`
answers `idempotency_keys: true` on every build that judges the header - a fact of the binary, so
it is true whatever the database or NATS answer, on a `503` too. A client tells a Dispatch that runs
this layer from one that ignores the header, and would run a retried write a second time, by one
or the other: the agents' client resends a failed write only against a Dispatch whose answer
carried the echo in the last five minutes, or whose `/healthz` said so in that time
(`packages/envoy-client/README.md`).

## Routes

`GET /api/v1` (no credential) is the authoritative list: every mounted `/api/v1` route with its
`method`, `path`, `auth` (`public`, `any`, `human`, or `bearer`), and `description`, sorted by path
then method. `envoy-dispatch routes` prints the same body from the table alone, with no database
or listener and never a test hook; the docs site's HTTP API reference is generated from it. The
table below is a summary. An unknown path under `/api`, `/v1`, `/auth`, `/ws`,
or `/healthz` is a JSON 404 `{"code":"NOT_FOUND","error":"no route for GET
/v1/issues","hint":"GET /api/v1 lists every route"}`, never the dashboard shell. When
`DISPATCH_ASSET_STORE_BUCKET` is configured, a missing local file under `/assets/` is read whole
from the same key in the retained-assets bucket before Dispatch answers; it never reads
`index.html` or a non-asset path from that bucket. An absent object remains
`404 {"error":"not found"}` and any other store failure is a 502 with no cache header; without the
setting every local asset miss remains the 404. The dashboard's
pages (`index.html` and the shell served for a browser route) carry `Cache-Control: no-cache` and
an `ETag` of their bytes, no `Last-Modified`, and its hashed `/assets` files
`public, max-age=31536000, immutable`, so a browser keeps no page from another build.


In this table, header identity means only the test/local harness mode described above. Production
Dispatch uses cookie identity through the sign-in pool.
| Path | Method | Identity | Purpose |
| --- | --- | --- | --- |
| `/api/v1` | GET | none | List every `/api/v1` route with its auth and purpose. |
| `/auth/start` | GET | none | Start Google sign-in at the sign-in issuer: the Cognito sign-in pool, or Google itself for `DISPATCH_SIGNIN_DOMAIN`, asking for offline access with `prompt=consent` and the domain as `hd`. `503 SIGNIN_UNCONFIGURED` on a server with no `DISPATCH_SIGNIN_*`. |
| `/auth/callback` | GET | sign-in state | Exchange the issuer's code and sign in the person `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` admits: under a group, named by the email in their pool username, anyone outside the group refused with a 403 page naming them; under a domain, named by the verified `email` of a Google account whose `hd` is the domain, anyone else refused with a 403 page giving the reason. Record the person and the refresh token (and, under a domain, the account's `sub`, ending the address's sessions when another account held it), issue the cookie. `409 SIGNIN_SUPERSEDED` and no cookie when, under a domain, another account signed in to the address between this sign-in's record and its cookie. |
| `/auth/logout` | POST | cookie or trusted header | End the person's Dispatch sessions, forget their refresh token, and clear the session cookie. |
| `/auth/whoami` | GET | cookie or trusted header | Return the resolved person, `{kind: "user", login, event_head}` with `login` their lowercase email and `event_head` the newest committed event id when the sign-in check answered. The page starts its events subscription from that id. The sign-in check comes first: a caller who is not signed in is `401 NO_IDENTITY` and the head is never read, and a signed-in caller whose head cannot be read is `503 EVENT_HEAD_UNAVAILABLE`. |
| `/auth/_dev/signin` | GET | none; `DISPATCH_DEV_SIGNIN=1` only; loopback peer, no forwarding header | Sign in the person `login` names, lowercased, without the sign-in issuer: `302` to the sanitized `next` (default `/`) with the session cookie. `400 DEV_SIGNIN_INPUT` without `login`, `403 DEV_SIGNIN_FORBIDDEN` for a non-loopback peer or a request carrying `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP` or `Via`. Each mint logs at WARN with the email and the peer. Not mounted otherwise. |
| `/api/github/rest/...` | GET | cookie or trusted header | Read GitHub as the GitHub App installation that covers the repository: a pull request (`repos/{o}/{r}/pulls/{n}`), an issue (`repos/{o}/{r}/issues/{n}`) or a commit's check runs (`repos/{o}/{r}/commits/{sha}/check-runs`), the only reads the web app makes. Every person shares the App's credential, which reads far more of a repository than that, so any other method is `405 METHOD_NOT_ALLOWED`, any other path `404 GITHUB_PATH_REFUSED`, and a repository the App is not installed on, or no App key, `503 GITHUB_TOKEN_UNAVAILABLE`. |
| `/healthz` | GET | none | Report that the process serves, Postgres answers within `store.healthProbeTimeout` (two seconds) on the health pool — a dedicated one-connection pool, never the shared one — NATS is connected where configured, and the file store's bucket answers a two-second `HeadBucket` where `DISPATCH_FILE_STORE_BUCKET` is set (`files`; null when it is not). A database that stops answering is `503` with `db: false` inside that bound, never silence, and the reason is logged. `files` is reported and never decides `ok` or the status: a bucket that stops answering is `200` with `files: false`, so a bucket outage fails uploads and downloads and nothing else, and a deploy check that wants the bucket asserts `files: true` itself. Two seconds fits the tightest prober here, the three-second compose healthcheck and deploy script, as well as the ALB's five. The body also names what is deployed: `commit`, the legion commit the image build stamped (the Dockerfile's `LEGION_COMMIT`; `null` in an unstamped build), and `schema_version`, the highest migration `schema_migrations` records, read by the same probe (`null` when `db` is false). |
| `/api/v1/artifacts/_test/quiesce` | POST | cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1` | Close every live document and wait for the settlements in flight; not mounted otherwise. |
| `/api/v1/artifacts/{id}/_test/outside-schema` | POST | cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1` | Write the crafted malformed tree e2e uses; not mounted otherwise. |
| `/api/v1/live/_test/disconnect` | POST | cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1` | Close every open live socket with 1012, as a restart does; not mounted otherwise. |
| `/api/v1/live/_test/rotate` | POST | cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1` | Close every open live socket with 4001 (`max age`); not mounted otherwise. |
| `/api/v1/agents/{session_id}/stream/_test/publish` | POST | cookie or trusted header (human only); `DISPATCH_TEST_HOOKS=1` | Feed the in-memory agent relay; `as=drop` ends agent views as `dropped`, so the page subscribes again and rebuilds from the replay; not mounted otherwise. |
| `/api/v1/writes/_test/slow` | POST | cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1` | `{method, path, delay_ms}` arms or replaces a delay for that method and exact request path, and `delay_ms: 0` removes it: every keyed run of it waits `delay_ms` on its handler's context once its key is claimed, and a context that ends during the wait answers `503 SLOW_WRITE_CUT_OFF` with the handler never run; a bad body is `400`. Not mounted otherwise. |
| `/ws/live` | GET | cookie, trusted header, or bearer | One WebSocket per browser tab, serving delivery, events, and agent subscriptions. A socket rotates with 4001 at a random age from 30 to 60 minutes; it closes with 1012 on restart, 1008 when its caller stops resolving, and 1013 when the hub queue fills. |
| `/api/v1/inbox?project=&assignee=` | GET | cookie or trusted header (human only) | List open asks newest-first, including their issue key, title, and assignee. `assignee=me\|unassigned\|<email>` keeps asks on issues held by the caller, by nobody (project-document asks included), or by that person; an email nobody has signed in with is `400 ASSIGNEE_NOT_ALLOWED`. |
| `/api/v1/me/answers?limit=&offset=` | GET | cookie or trusted header (human only) | List the caller's answers and replies on asks, newest first. A row is either an answer with `current`, or a reply, and carries the ask's question, kind, state, owner and deep link. Answers come from user-authored `ask.answered` events whose actor is the answer's user; a restored answered block adds no row. `limit` is 1–200 (default 50), `offset` is 0 or more, and an invalid value is `400 INVALID_QUERY`. |
| `/api/v1/me/replies?limit=&offset=&issue=\|artifact=` | GET | cookie or trusted header (human only) | List the threads the caller wrote in where someone else has written since the caller last read them, newest reply first, less an open ask the Inbox lists. A row names the thread, its owner, its subject, its root's deep link and the newest reply's author, first line and time. `issue` or `artifact` narrows to one owner (both is `400 INVALID_QUERY`); `limit` is 1–200 (default 50). |
| `/api/v1/me/threads/{id}/read` | PUT | cookie or trusted header (human only) | Move the caller's read mark on one thread forward to `{read_through}`; it never moves back, a value more than a minute ahead of the server is `400 INVALID_STATE`, and a non-uuid id is `400 THREAD_ID_INPUT`. Answers `204`, writing nothing for a thread the caller never wrote in. |
| `/api/v1/agents` | GET | cookie, trusted header, or bearer | List live Envoy sessions (`session_id`, `title`, `dir`, `machine_id`, `roles`, `capabilities`, `last_seen`), newest first; `503 ENVOY_UNAVAILABLE` when the listener cannot be reached. |
| `/api/v1/projects` | GET | cookie, trusted header, or bearer | List projects (`key`, `name`, `open_asks`, `created_at`), ordered by key. |
| `/api/v1/projects` | POST | cookie, trusted header, or bearer | Create a project from `key` and `name`, for a person or an agent session alike; rejects a key that is not a project key, an empty name, a name holding a control character or a bidi embedding, override or isolate (U+202A-U+202E, U+2066-U+2069), and a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF); combining marks, format characters, private-use characters and whitespace don't count) with `400 INVALID_PROJECT`, a name over 80 characters (`contracts.ProjectNameMax`, UTF-16 units after the trim) with `400 CAP_EXCEEDED`, and a duplicate key with `409 PROJECT_EXISTS`. |
| `/api/v1/settings/repo-projects` | GET | cookie, trusted header, or bearer | List repository-to-project mappings, each with `default`, whether it is its project's default repository. |
| `/api/v1/settings/repo-projects/{owner}/{repo}` | PUT, DELETE | cookie, trusted header, or bearer | Create or replace, or remove, a repository mapping; PUT takes `{project, default?}`, and `default: true` moves the project's one default to it. A mapping an issue names as its `repo` is neither deleted nor moved: `409 REPO_PROJECT_IN_USE`. `AGENTS.md` gives the rules and the `settings.repo_project.updated` events. |
| `/api/v1/settings/project-default-assignees` | GET | cookie or trusted header (human only) | List every project that names a default assignee, by project key, as `[{project, assignee}]`; a project with no default is left out. Human-only, since the default names a person. |
| `/api/v1/projects/{key}/default-assignee` | PUT | cookie or trusted header (human only) | Set a project's default assignee `{assignee}` (the email of a person who has signed in, or `null` to clear it): who an issue created with no assignee, by nobody, under no parent or under a parent with nobody on it, goes to — the last step of the create chain. `400 ASSIGNEE_NOT_ALLOWED` for an email nobody has signed in with, `400 INVALID_PROJECT` for a body without `assignee` or one whose `assignee` is neither a string nor null, `404 NOT_FOUND` for an unknown project. Human-only: no agent surface sets it. |
| `/api/v1/delivery/session-groups` | POST | cookie, trusted header, or bearer | Create a Delivery session group `{name, sessions}`: its sessions' pull requests take it as their Parent agent, `group:<id>`. `409 SESSION_IN_GROUP` for a session another group holds, `409 SESSION_GROUP_NAME_TAKEN` for a name another group has (ignoring case), `409 SESSION_GROUP_NAME_IS_SESSION_TITLE` for a name that is the latest title of a session outside the group, `400 SESSION_GROUP_INPUT` for a blank name or no sessions. The timeline lists every group (`session_groups`). |
| `/api/v1/delivery/session-groups/{id}` | PATCH, DELETE | cookie, trusted header, or bearer | Rename a session group, add sessions to it or remove them `{name?, add?, remove?}`, with the create route's refusals and `400 SESSION_GROUP_INPUT` for an edit that would leave it empty; or delete it, its sessions showing as themselves again. `400 SESSION_GROUP_ID_INPUT` for an id that is not a UUID, `404 NOT_FOUND` for one no group has. |
| `/api/v1/me/agent-tokens` | GET, POST | cookie or trusted header (human only) | List personal-token metadata or mint a personal agent token. |
| `/api/v1/me/agent-tokens/{id}` | DELETE | cookie or trusted header (human only) | Revoke a personal agent token. |
| `/api/v1/users` | GET | cookie or trusted header (human only) | Everyone who has signed in, as `{users: [{login}]}` sorted by email — the assignee picker's options. |
| `/api/v1/whoami` | GET | cookie, trusted header, or bearer | Who the server takes the caller for: `{kind: "user", login}` for a person (`login` their lowercase email), `{kind: "agent", owner, service}` for a bearer (`owner` is a personal token's owner by lowercase email, null for the shared token; `service` is a verified service-account token's Kubernetes subject, null for every other bearer). |
| `/api/v1/issues?project=&status=&parent=&priority=&updated_since=&route_status=&limit=&offset=` | GET | cookie, trusted header, or bearer | List issue summaries: every matching issue as an array, or, with `limit` (1–250) or `offset` (0 or more; alone it pages 50), one page `{issues, total, limit, offset}` cut after every filter, `total` counting the issues they match. A repeated, blank, non-integer or out-of-range `limit` or `offset`, or any `cursor`, is `400 INVALID_QUERY` naming the parameter. The order is status, rank, creation time and key, so consecutive offsets cover the listing once while it does not change between reads; an issue that enters or leaves what the filters match, or whose status or rank changes, between two reads shifts rows across a page boundary, so one issue is served twice and another never. Only the unpaged array is an exact set in one read. Filters are optional; `updated_since` is RFC3339 and inclusive, matching issue changes and later issue events. `priority` repeats (`priority=0&priority=1`), each value `0`–`3` or `none` for an issue with no priority; any other value is `400 INVALID_PRIORITY`. `route_status` (`live`, `no_holder` or `unknown`; anything else is `400 INVALID_ROUTE_STATUS`) keeps the open issues whose route is in that state, whatever their priority; `live` or `no_holder` is `503 ENVOY_UNAVAILABLE` when the listener does not answer. Summaries contain `key`, `title`, `status`, `priority`, `parent`, `assignee`, `route`, `route_status`, `route_holder`, `updated_at`, `last_seq`, and `open_asks`. Every issue read (this list, `?pinned=true`, and `GET /api/v1/issues/{key}`) resolves `route_status` from one listener `GET /v1/sessions` per request, stored nowhere: `live` (a live session holds the role, or the session is live; `route_holder` names it), `no_holder` (nobody live holds the role, or the session is not live), `unknown` (the listener did not answer), or null with no route. |
| `/api/v1/search?q=&project=&limit=&offset=` | GET | cookie, trusted header, or bearer | Full-text search over issue titles, latest document text, comments, asks, and messages; each kind ranked on its own and merged by reciprocal rank fusion (above), with `<mark>` snippets and SPA `href`s. One page `{results, total, reachable, limit, offset, took_ms}`: `limit` 1–50 (default 20), `offset` 0 or more (default 0), `total` every match, `reachable` the hits the pages can return (each kind's best 100). An under-two-character query returns `400 INVALID_QUERY`; a stop-word-only query returns `200` with no results; `400 CAP_EXCEEDED` over 1,000 characters (`contracts.SearchQueryMax`, UTF-16 units after trimming), since the query rides in the URL; `400 INVALID_PROJECT` for a project that is not a project key (none searches every project); `400 INVALID_LIMIT`; `400 INVALID_OFFSET`. Keyword search finds a text whose search vector would pass Postgres's limit on one vector by the words in its opening part only ([Search](#search)). |
| `/api/v1/issues/{key}/references` | GET | cookie, trusted header, or bearer | Read the issue's eight-hop artifact reference closure. An `If-None-Match` value equal to the response ETag returns `304`. |
| `/api/v1/references?to=\|from=&kind=&since=` | GET | cookie, trusted header, or bearer | Edges of one node in the reference graph, newest first and cross-project: exactly one of `to` (backlinks) or `from` (links), each a `dispatch://` reference; `kind` filters a csv of edge kinds; `since=<events.id>` keeps mentions introduced after it (structural edges excluded). Each edge carries the other `node`, an `excerpt` (the containing block for a document mention), `created_at`, and `source_seq`. `400 INVALID_REFERENCE` / `INVALID_KIND` / `INVALID_SINCE`; `404` for a node that does not exist. |
| `/api/v1/issues` | POST | cookie, trusted header, or bearer | Create an issue and its primary document. Omitting or leaving `spec` blank gives an empty primary document at version 1. `repo` (`owner/name`) is the issue's own repository and must be mapped to its project: `400 INVALID_ISSUE` when it is not `owner/name`, `400 REPO_NOT_MAPPED` when it is not mapped. A title over 1,000 characters (`contracts.IssueTitleMax`, UTF-16 units after trimming) is `400 CAP_EXCEEDED` before the duplicate check. Refuses a title that near-duplicates an issue in the project with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true; external references skip the check. A spec whose ask block breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`, and a spec whose markdown makes more than 65,536 elements (weighed as Element weights below lists) is `413 CAP_EXCEEDED`, naming the line that passes the limit; so is one whose markdown as stored (what `.../text` answers) would be longer than 1 MiB or make more than 65,536 elements. |
| `/api/v1/issues/{key}` | PATCH | cookie, trusted header, or bearer | Update an issue's `title`, `status`, `priority`, `labels` (or `labels_add`, which adds to the labels the issue holds when the write commits; not with `labels`), `route`, `external_links`, `parent`, `rank`, `assignee`, `components` or `repo` (`null` clears it); `AGENTS.md` gives each field's rules. A title is trimmed, and one over 1,000 characters is `400 CAP_EXCEEDED`, as on creation. `expected_sequence` makes the write conditional: `409 ISSUE_CHANGED` when the issue's last event is no longer that sequence. |
| `/api/v1/issues/{key}/asks` | POST | cookie, trusted header, or bearer | Create an optionally anchored ask. An anchor is exactly `{artifact, quote, occurrence?}` for a server-written quote mark or `{artifact, mark_id}` for a mark already written by a browser. A quote mark carries the ask's id and who asked, which no rendering shows and every load of the document builds: the marks a document's comments, suggestions and asks hold on its text carry at most 1 MiB of ids and authors, and a quote anchor that would leave them past it and carrying more is `413 CAP_EXCEEDED`, naming the bound and both sizes. |
| `/api/v1/issues/{key}/asks?state=&thread=` | GET | cookie, trusted header, or bearer | List an issue's asks, open and/or answered (`state`: `all` default, `open`, or `answered`). Each row carries `last_reply`, the newest comment in the ask's thread or null. `thread=1` adds `thread`, its `replies`, `edits`, `answers` and `followers`, batched over the list so no card fetches its own; without it no `thread` is returned, which is what the readers that render the ask alone want from this unpaged list. Any other `thread` value is `400 INVALID_ASK_THREAD`. |
| `/api/v1/asks/{id}` | GET | cookie, trusted header, or bearer | Read an ask with its reply thread, edits, followers and `answers`, every user-authored answer oldest first. |
| `/api/v1/asks/{id}` | PATCH | cookie, trusted header, or bearer | Edit one or more of `question`, `options`, `multiple`, or `urgency` on an open ask. A bearer caller must be the asking session; a human may edit any open ask. The response records `edited_at` and emits `ask.edited` with the current ask, prior mutable fields, and `edited_by`. Anchors are selected when the ask is created and cannot be changed by this route. On a block ask the `:::ask` block is written in the same transaction and the row takes the block's parsed values, so the edit versions the document once and no settlement reverts it. Only the named fields are written: `urgency`/`multiple` alone go through the attribute path and leave the body's nodes, marks and inner block ids untouched, so an untouched question keeps its formatting, links and comment anchors; naming `question` or `options` replaces that part with the markdown pipeline's own parse, and anchors inside the replaced text move as for any document edit. A field named but unchanged is not rewritten, so an idempotent retry of the whole ask writes nothing, versions nothing, keeps every anchor and returns 200. Text the block cannot carry back unchanged — an option label containing `": "`, the separator between a label and its description, is one example — is `400 ASK_BLOCK_TEXT` naming the field, with nothing written. Text past what one write's markdown may make - weighed before it is rendered, an element for each backslash, `*`, `_`, `~`, backtick, `[`, `]` and `<` and two for each line feed, then as its markdown parses - is `413 CAP_EXCEEDED`. An edit of a block ask that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, is `413 CAP_EXCEEDED` and the ask keeps its text. |
| `/api/v1/asks/{id}/answer` | POST | cookie or trusted header | Answer an open ask, or change the current answer with `expected_answer_at` set to that answer's `at`. Without it an answered ask is `409 ASK_CLOSED`. A stale value, or one sent for an open ask, is `409 ASK_ANSWER_CHANGED`; a different person is `403 NOT_ANSWERER`; an approval is `409 ASK_APPROVAL_REVIEW`. A change appends another `ask.answered` with `previous_answer`. An approval ask is moved to each document version before the human sees it, and an `ASK_EDITED` response means the question changed after the human reviewed it, so they reload and answer the moved request. On a block ask the answer is written into its block, and a changed answer without text removes the block's old `answer`. Its words and the options it chooses, whose labels are the asker's text, are weighed: where they would leave the document's markdown longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, the ask still takes the whole answer and the block is written without them, saying who answered and when. The answer's state and who gave it and when are not weighed, so the asks of a document already past those limits can still be answered. The answer is kept on the ask as well: a block that leaves the document and returns gets it back at settlement, which weighs each returning answer on its own in document order and keeps one on the ask alone, the block saying who answered and when, while writing it back would take the document past those limits and make it bigger. |
| `/api/v1/asks/{id}/resolve` | POST | cookie, trusted header, or bearer | Retract or self-resolve an open ask with a recorded reason. On a block ask the block's `state` is written with it, and that is not weighed, so an ask on a document past what one upload may hold can still be resolved. A reason beginning `removed from the document in version`, which marks a retraction settlement wrote and would have the retract undone when the block returns, is `400 INVALID_RESOLUTION`. |
| `/api/v1/issues/{key}/comments?artifact=` | GET | cookie, trusted header, or bearer | List comments, optionally limited to an artifact ID. |
| `/api/v1/issues/{key}/comments` | POST | cookie, trusted header, or bearer | Create a comment, root-level reply, or suggestion. An anchored comment uses the same quote-or-mark-ID shape as an ask; replies inherit their root's anchor and send none. A comment's record in its document's margin - its body and replies, or a suggestion's and its replacement - holds at most 256 KiB of text, and a document's margin 1 MiB: a comment, reply or suggestion that would leave either past its bound and bigger is `413 CAP_EXCEEDED`, naming the bound and both sizes. A quote anchor that would leave the marks on the document's text carrying more than 1 MiB of ids and authors, and more than they carried, is `413 CAP_EXCEEDED` too, as for an ask's. |
| `/api/v1/comments/{id}` | GET | cookie, trusted header, or bearer | Read a comment and its reply chain. |
| `/api/v1/comments/{id}` | PATCH | cookie or trusted header | Edit a comment body. Human authors only. An edit that would leave the comment's margin record or its document's margin past its bound (above) and bigger is `413 CAP_EXCEEDED`. |
| `/api/v1/comments/{id}/resolve` | POST | cookie, trusted header, or bearer | Resolve a comment. |
| `/api/v1/comments/{id}/reopen` | POST | cookie or trusted header | Reopen a resolved thread-root comment. |
| `/api/v1/comments/{id}/accept` | POST | cookie or trusted header | Apply and accept an anchored suggestion. An accept that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, is `413 CAP_EXCEEDED` and leaves the suggestion open. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and leaves the suggestion open; one removed after it answers `200` with `lost: true`. |
| `/api/v1/comments/{id}/reject` | POST | cookie or trusted header | Reject a suggestion. |
| `/api/v1/issues/{key}/messages` | POST | cookie, trusted header, or bearer | Post an issue message. |
| `/api/v1/issues/{key}/messages` | GET | cookie, trusted header, or bearer | List an issue's message threads: every root message, oldest first, unpaged, each with its delivery attempts and its replies. |
| `/api/v1/issues/{key}/artifacts` | GET, POST | cookie, trusted header, or bearer | List issue artifacts or create a version from a multipart `file` or JSON `{name, content, summary?, actor?}`. The JSON form requires `Content-Type: application/json`. A `text/markdown` file is a document. Any other file's kind is read from its bytes, never from the part's declared `Content-Type`: it is `image` only when its bytes are a PNG, JPEG, GIF or WebP, stored with `mime` the type its bytes are (a JPEG declared `image/png` is `image/jpeg`), and otherwise a `file` under its declared type, an SVG among them, so no client stores other bytes as an image. A version whose kind is not its artifact's is `400 ARTIFACT_KIND_MISMATCH`. An ask block whose body breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes. A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose markdown makes more than 65,536 elements (weighed as Element weights below lists), naming the line that passes the limit; a document or version whose markdown as stored (what `.../text` answers) would be longer than 1 MiB and longer than the document's was, or make more than 65,536 elements and more than the document's did, or that would leave a live state ygo could not decode as one update; or whose formatting is more items than one document update can store (1,048,576), naming the count. A file the configured bucket refuses is `502 FILE_STORE_UNAVAILABLE`, with no version written. |
| `/api/v1/projects/{key}/artifacts` | GET, POST | cookie, trusted header, or bearer | List non-primary project artifacts (or only unlinked documents with `?unlinked=true`), or create an unlinked project artifact, its kind read as an issue upload's is. An ask block whose body breaks its content rule (`paragraph+ bullet_list?`: one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes. A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose markdown makes more than 65,536 elements (weighed as Element weights below lists), naming the line that passes the limit; a document or version whose markdown as stored (what `.../text` answers) would be longer than 1 MiB and longer than the document's was, or make more than 65,536 elements and more than the document's did, or that would leave a live state ygo could not decode as one update; or whose formatting is more items than one document update can store (1,048,576), naming the count. A file the configured bucket refuses is `502 FILE_STORE_UNAVAILABLE`, with no version written. |
| `/api/v1/artifacts/{id}` | GET | cookie, trusted header, or bearer | Read an artifact, its versions, and incoming references. `{id}` must be a UUID. Every version on every artifact read and list (this route, the issue and project artifact lists, the issue read, the reference reads, and each version write's answer and event) carries `size`: the bytes it stores, a file's or an image's bytes or a document's markdown, read from the row's own length without rendering the document or loading its history. |
| `/api/v1/artifacts/{id}/rebuild` | POST | cookie or trusted header (human only) | Rebuild a document only when its durable history cannot load (`409 DOCUMENT_UNLOADABLE` on its reads): deletes its document updates, checkpoints, and snapshots, then writes one fresh update from the latest saved version or optional `{markdown}`. A supplied markdown source that changes the document writes the next immutable version, emits `artifact.version`, and moves an open approval request to that version, where it waits on its agent, as every version does; an omitted source keeps the existing latest-version behavior. The rebuild, that version, its event and the move commit in one transaction, and every refusal comes before anything is written: a live room is `409 DOCUMENT_LIVE`; a healthy, non-resident document is `409 DOCUMENT_LOADS`; supplied markdown on a closed issue's document is `409 ISSUE_CLOSED`; supplied markdown held to a new document's limits - more than 65,536 elements, or stored as markdown longer than 1 MiB or making more than 65,536 elements - is `413 CAP_EXCEEDED`. The latest saved version rebuilds whatever it weighs, except a document whose tree is more items than ygo decodes in one update (1,048,576), which no document can hold and which is `413 CAP_EXCEEDED` naming the count, from either source. A refused or failed rebuild leaves the document unchanged. It answers the report, whose `source_version` is the version the rebuilt document holds: its latest saved version, or the version supplied markdown wrote. |
| `/api/v1/artifacts/{id}/text` | GET | cookie, trusted header, or bearer | Read a live document's markdown. `{id}` must be a UUID. A stored tree outside the Proof schema is `409 DOC_SCHEMA`, and a stored history that cannot load `409 DOCUMENT_UNLOADABLE` (Document errors). |
| `/api/v1/artifacts/{id}/versions/{n}` | GET | cookie, trusted header, or bearer | Read a document version or download a blob. `{id}` must be a UUID. A file whose bytes are in the bucket is `502 FILE_STORE_UNAVAILABLE` when the bucket does not answer, `503` on a server with no bucket configured. A file or image version, whoever owns it, is served with `Cache-Control: private, max-age=31536000, immutable` beside its `ETag` (the bytes' sha256), since a version's bytes never change and the route is authenticated; a document version, and every error, carries no cache header. |
| `/api/v1/artifacts/{id}/versions` | POST | cookie, trusted header, or bearer | Create a named live-document version. `{id}` must be a UUID. |
| `/api/v1/artifacts/{id}/edits` | POST | cookie, trusted header, or bearer | Apply document edit operations. `{id}` must be a UUID. An edit is `400 INVALID_ASK_BLOCK` when an ask it writes or changes breaks its content rule (`paragraph+ bullet_list?`) or holds what settlement cannot read; an ask it carries through unchanged is not its to refuse. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. A batch whose markdown (its inserts and replaces together) makes more than 65,536 elements, or that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, or a live state ygo could not decode as one update, is `413 CAP_EXCEEDED` with the refusal itself as the error, and writes nothing. |
| `/api/v1/artifacts/{id}/approval-requests` | POST | cookie, trusted header, or bearer | Ask a human to approve a document's latest settled version. The question is `Approve <name> (version <N>)?` followed by the optional `summary` (blank is `400 SUMMARY_INPUT`; past the ask cap at the longest version a request can reach, ten digits, is `400 CAP_EXCEEDED` naming `summary`). A request on a document a human approved earlier names that approval in `ask.approval.approved` (`{version, at, by}`); a first request carries no `approved` key. One open approval row follows every document version in place, rewording its question and emitting `ask.edited`; while its `requested_version` is below the new version it waits on the agent. Only the first move since the request was opened or handed back notifies, even when a thread reply had already left it waiting on the agent; a later move, while `requested_version` is already below the version it named, is `quiet: true`, `notify: false`, and reaches no follower. A version a human's answer to a decision block writes, when that answer is the whole change since the version he approved (who answered what is the document's asks rows, and an ask block no row of the document backs, a copy, one settlement could not read, one whose server-owned attributes are not as settlement writes them from its row, or a block id the version repeats carries nothing), carries his approval to it in the same transaction - an `approved` review in his name whose `reason` names the carry, and `artifact.approved` - and answers the open request `Approve` in his name after the move (`ask.answered`, the review's `ask_id`), so nothing is requested again for it. Calling this route again while the row waits on the agent - moved, or a thread reply newer than its last hand-back holds the turn - hands it back to the human: a summary that changes its question rewords it first (`ask.edited`; an omitted summary keeps its prior one), then `requested_version` becomes the latest version and `ask.handed_back` is emitted, leaving `edited_at` as it was. While the row waits on the human, the same summary or none is a repeat that answers `200` with no event, and a different one is `409 APPROVAL_WAITS_ON_HUMAN` and changes nothing, since it would rewrite the card the human is reading. It answers `201` when it wrote anything, with the document's `approval` as the call left it (`waiting_on` while awaiting). |
| `/api/v1/artifacts/{id}/asks?state=` | GET, POST | cookie, trusted header, or bearer | List or create asks on an unlinked document. |
| `/api/v1/artifacts/{id}/comments` | GET, POST | cookie, trusted header, or bearer | List or create comments and suggestions on an unlinked document. |
| `/api/v1/artifacts/{id}/events` | GET | cookie, trusted header, or bearer | Read an unlinked document's events. |
| `/api/v1/artifacts/{id}/references` | GET | cookie, trusted header, or bearer | Read an artifact's outgoing and incoming reference edges. |
| `/api/v1/issues/{key}/artifacts/{slug}` | GET | cookie, trusted header, or bearer | Read an issue artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/issues/{key}/artifacts/{slug}/text` | GET | cookie, trusted header, or bearer | Read an issue artifact's live document markdown. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions/{n}` | GET | cookie, trusted header, or bearer | Read an issue artifact version or download its blob, as `/api/v1/artifacts/{id}/versions/{n}` does. |
| `/api/v1/issues/{key}/artifacts/{slug}/versions` | POST | cookie, trusted header, or bearer | Create a named issue-document version. |
| `/api/v1/issues/{key}/artifacts/{slug}/edits` | POST | cookie, trusted header, or bearer | Apply issue-document edit operations. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/projects/{key}/artifacts/{slug}` | GET | cookie, trusted header, or bearer | Read an unlinked project artifact and its incoming references. `{slug}` is resolved within `{key}`. |
| `/api/v1/projects/{key}/artifacts/{slug}/text` | GET | cookie, trusted header, or bearer | Read an unlinked project document's live markdown. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions/{n}` | GET | cookie, trusted header, or bearer | Read an unlinked project document version or download its blob, as `/api/v1/artifacts/{id}/versions/{n}` does. |
| `/api/v1/projects/{key}/artifacts/{slug}/versions` | POST | cookie, trusted header, or bearer | Create a named project-document version. |
| `/api/v1/projects/{key}/artifacts/{slug}/edits` | POST | cookie, trusted header, or bearer | Apply project-document edit operations. A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing; one removed after it answers `200` with `lost_ops`. |
| `/api/v1/agents/{session_id}/artifacts` | POST | cookie, trusted header, or bearer | Upload a file or image to an agent's conversation: a multipart `file` (+ `name`, `summary?`, `actor?`) with an issue upload's caps, errors and file-store write, its kind read from its bytes as an issue upload's is (an `image` only for a PNG, JPEG, GIF or WebP, stored under the type its bytes are; any other file a `file` under its declared type), stored with `session_id` set, `issue_key` null, `project` `""` and `ref_key` `agent/<session_id>/<slug>`, addressed `dispatch://agent/<session_id>/artifact/<slug>`; it appends no event, since every event belongs to an issue, a document or a project. There is no list route: a conversation's pictures are reached through the references its messages carry, never enumerated. An agent's conversation holds files and images only, so a markdown document (JSON `content` or a `text/markdown` file) is `400 ARTIFACT_INPUT` ("an agent's conversation holds files and images; a document belongs to an issue or a project"). A `session_id` that is empty or holds `/`, `?`, `#`, whitespace, a control character or a character that ends a reference in text (`<>"'` and a backtick, `[`, `]`, `\|`) is `400 INVALID_SESSION_ID` on all three agent routes. Such a file takes no comment, ask, event read or subscriber through `/api/v1/artifacts/{id}/...`, which is `400 ARTIFACT_AGENT_OWNED`. |
| `/api/v1/agents/{session_id}/artifacts/{slug}` | GET | cookie, trusted header, or bearer | Read an agent's conversation's file or image and its versions by slug. |
| `/api/v1/agents/{session_id}/artifacts/{slug}/versions/{n}` | GET | cookie, trusted header, or bearer | Download one version of an agent's conversation's file or image, as `/api/v1/artifacts/{id}/versions/{n}` does, with its cache header. |
| `/...` | GET | none | Serve the dashboard static files. |

## Dispatch topics

Document events publish retained envelopes on
`notifications.dispatch.document.<PROJECT>.<slug>.<type>`. Use
`go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1`
to print one matching envelope from the `envoy.natsUrls` configured in `envoy.json`.
`natstail` owns nothing on the bus - it neither creates nor updates
`ENVOY_NOTIFICATIONS` - and refuses a NATS server that is not this machine's:
prefix the command with `ENVOY_ALLOW_REMOTE_NATS=1` where `envoy.json` names a
shared server, as an agent devbox's does. It connects as the nkey user
`NATS_NKEY_SEED_FILE` (or `NATS_NKEY_SEED`) names, never `envoy.natsSeed`, so a
run against a NATS that requires a user sets `NATS_NKEY_SEED_FILE` too.

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

## Element weights

One write's markdown, and the markdown a document stores, make at most 65,536 elements
(`pmdoc.MaxDocumentElements`), weighed node by node over the tree goldmark parses, front matter
apart. Every route that takes caller markdown refuses past that with `413 CAP_EXCEEDED`, and its
message lists these weights, written once in `pmdoc/elements.go` (`weighed`):

| Node | Weight |
| --- | --- |
| a block (paragraph, heading, quote, list, list item, code block, rule, table, table row, footnote definition, typed block) | 3 |
| the empty block an empty container is read as holding: the paragraph of a list item, quote, footnote definition or typed block holding nothing (or of a list item opening with another block), and the row of a table with no body row | 3 more |
| a table cell | 4 |
| a hard line break, beside its line of text | 3 |
| an image | 3 |
| a piece of inline HTML, a tag or a comment | 2 |
| an autolink | 2 |
| a footnote reference | 2 |
| an escape: a backslash before punctuation, or a character reference | 1 |
| a piece of inline syntax, a mark, a line of text | 1 |

Every node the Proof tree makes other than a text is an element of the live document carrying
attributes, and costs the server about what two inline nodes do; each node class that weighed less
let the heaviest document of it the limit admitted hold past the 256 MiB one request may, until it
was weighed as above. `TestEveryNodeOfTheSchemaWeighsWhatItMakes` holds every node type of the
schema to at least two elements for each element its cheapest markdown makes and one for each text,
and fails for a node type the schema gains until it is given that markdown and a weight. What each
node type weighs:

| Proof node | Made of | Weight | Attributes |
| --- | --- | --- | --- |
| `doc` | the document | 0, one a document | none |
| `frontmatter` | front matter | 0, one a document, apart from the count | none |
| `paragraph` | a paragraph, or a tight list item's text | 3, or 3 as an empty container's | block id |
| `heading` | a heading | 3 | block id, level, id |
| `blockquote` | a quote | 3, and 3 when empty | block id |
| `bullet_list`, `ordered_list` | a list | 3 | block id, spread, order |
| `list_item` | a list item; a task item's marker is 1 more | 3, and 3 when empty or opening with another block | block id, label, list type, checked, spread |
| `code_block` | fenced or indented code | 3 | block id, language |
| `hr` | a thematic break | 3 | block id |
| `table` | a table | 3, and 3 with no body row | block id |
| `table_header_row`, `table_row` | a row | 3 | block id |
| `table_header`, `table_cell` | a cell and its paragraph | 4 | block id, alignment, spans |
| `footnote_definition` | a footnote definition | 3, and 3 when empty | block id, label |
| `callout`, `ask` (typed blocks) | a `:::name{…}` block | 3, and 3 when empty | block id and the schema's attributes |
| `text` | a run of text; each mark a run carries is the inline syntax that opens it | 1 | none (marks) |
| `hardbreak` | a hard line break | 3, beside its line's 1 | isInline |
| `image` | an image; its alt text's nodes weigh 1 each | 3 | source, alt, title |
| `html` | a piece of inline HTML | 2 | the HTML |
| `footnote_reference` | a footnote reference | 2 | label |
## Input errors

| Status / code | Meaning |
| --- | --- |
| `400 NUL_CHARACTER` | Caller text holds a NUL character (U+0000), which PostgreSQL's text and jsonb cannot store: any string in a JSON body a route decodes, member names included, a multipart upload's field or markdown file, any route's path or query parameter, or the actor a document websocket's bearer names in `X-Dispatch-Actor` (refused before the upgrade). It covers every parameter, field and member a request carries, including ones the route does not read. The message names where it stands (`title`, `options[1].label`, `file`, `path parameter session_id`, `query parameter label`, `X-Dispatch-Actor.id`), with a NUL in a name the caller wrote shown as `\u0000`, and the character's position, counted from 1 in UTF-16 units; nothing is written. A NUL a browser edit puts in a live document is written as U+FFFD, which is what CommonMark reads one as, in the document's version and every read, a block's id included, an anchor's quote and an ask block's question and options, and a quote, a `# Title` quote and a `heading:` anchor are matched against that text. |
| `400 INVALID_UTF8` | Caller text holds a byte that is not UTF-8, which PostgreSQL's text cannot store: a multipart upload's field, its markdown file or a binary file part's `Content-Type` (`file Content-Type`), or any route's path or query parameter (`%FF`). JSON cannot carry one, since decoding writes it as U+FFFD. The message names the field, the byte and its position, as `NUL_CHARACTER` does; nothing is written. |
| `400 ARTIFACT_INPUT` | An artifact upload's multipart body the parser cannot read, such as a part header holding a control character, named with the parser's reason. A body past the upload's size limit stays `413 CAP_EXCEEDED`, and a file part the server cannot spool to its temporary directory is `500 INTERNAL`, logged. A markdown document uploaded to an agent's conversation is refused with this code too: it holds files and images, and a document belongs to an issue or a project. |
| `400 INVALID_SESSION_ID` | An agent artifact route's `session_id` is empty or holds `/`, `?`, `#`, whitespace, a control character or a character that ends a reference in text, so no reference could name what it owns; nothing is written. |

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
| `400 INVALID_MARKDOWN` | Uploaded document content cannot be represented by the Proof schema, such as a table row holding text in a cell past its delimiter row's width, which a pipe inside code or a link that is not backslash-escaped makes. A Markdown document nests at most 100 blocks (quotes, lists and their items, typed blocks, and footnote definitions), and one textblock's inline markdown at most 100 marks (emphasis, strong, strikethrough, links, images, and code); deeper content is refused naming the line. An accepted suggestion whose blocks nest within that bound but land deep enough that the document would nest past it is `INVALID_OP` on `replace_with`, naming how many blocks the result nests. Malformed edit replacements report `INVALID_OP`. |
| `409 DOC_SCHEMA` | A stored tree is outside the Proof schema, such as one holding a node more than 1,000 levels below the document, a node attribute value nesting more than 100 arrays and objects, or a mark attribute value nesting more than 99; settlement writes no version of such a tree. Every route that starts from that tree answers it alike, with one message - reading its text or blocks (`/text`, `/blocks`), editing it (`POST /edits`) and naming a version (`POST /versions`) - and a schema refusal of a tree the request itself produced is a `500`. Replace it by uploading markdown under the same document name; an upload preserves every open ask block, and is refused `400 INVALID_ASK_BLOCK` for an ask block that breaks the ask content rule, as a new document is. The document websocket admits no connection to such a room: it completes the upgrade and closes with code `4409` (reason `DOC_SCHEMA`, `DOCUMENT_SCHEMA_CLOSE_CODE` and `DOCUMENT_SCHEMA_CLOSE_REASON` in `packages/contracts`, generated into Go) before any sync, so a browser editor never receives a tree it would normalize and write back. |
| `409 DOCUMENT_UNLOADABLE` | A document's stored history does not decode, so nothing can read or open it. Rebuild it from its latest saved version (`POST /api/v1/artifacts/{id}/rebuild`); the dashboard offers that rebuild for this code alone. A room or store that could not serve the document stays `503 DOC_SERVICE_UNAVAILABLE`, which is retried, not rebuilt. |
| `409 DOCUMENT_LIVE` | A rebuild found the document resident in this server: open in an editor, or left by its last editor less than 90 seconds ago (a room stays resident a minute after its last peer leaves, and the sweep that evicts it runs every 30 seconds). Close its editors and retry two minutes later, or repair a schema-invalid tree by replacing it from markdown. |
| `409 DOCUMENT_LOADS` | A rebuild found that the document's durable history loads. Nothing was deleted; replace a schema-invalid document from markdown instead. |

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
