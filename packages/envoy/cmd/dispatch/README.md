# Dispatch HTTP server

Go binary serving the Dispatch dashboard and API.
People sign in through a Cognito sign-in pool or with Google directly, where the `DISPATCH_SIGNIN_*` settings configure one, and are named by lowercase email;
the web app's GitHub reads go to GitHub as the GitHub App.
Dispatch keeps application state in Postgres.

This file uses semantic line breaks: one sentence per line, and a long sentence may break after a clause.
A blank line separates list items.
Add a new item next to the item it belongs with, never at the end of a list unless it belongs there, so two pull requests that add items or change different sentences do not conflict.
`bun scripts/semantic-line-breaks.ts packages/envoy/cmd/dispatch/README.md` checks the layout, and `--write` fixes it.

## Required configuration

Every Dispatch setting the server and its subcommands read is a row of one table, `cmd/dispatch/settings.go`:
`main` reads each row once, every reader takes its value from that read, and a test fails on any other environment read in `cmd/dispatch` or `internal/dispatch`.
`envoy-dispatch settings` prints the table (name, `_FILE` form, default, whether it is required, and a one-line description) without a database or a listener, and the docs site's Dispatch configuration reference is generated from it:
those are where every setting is listed.
The items below add only what a setting's one line cannot say.

Each item below names a variable, then its purpose.

- `DATABASE_URL`

  Postgres connection string.
  Dispatch applies embedded migrations before serving.
  The pool size is fixed in code (`store.sharedPoolSize`), so a connection string carrying `pool_max_conns` is refused at startup; remove the parameter.

- `DISPATCH_AGENT_TOKEN`

  One value, or several separated by whitespace, so the shared token can rotate with an overlap:
  write the new value followed by the old one, move every consumer to the new one, then write the new value alone.
  The first is the current value; the API accepts every value, comparing a bearer with each in constant time.
  The document websocket takes no bearer at all: it is a person's cookie connection only.
  Whitespace at either end is ignored; startup refuses an empty entry (two whitespace characters in a row, a CR LF among them) or a repeated one, naming its position, never its value.
  A request that authenticates with a value after the first logs one WARN line, `dispatch: request authenticated with a previous shared agent token`, with `entry` (the value's position), `address` (the rightmost `X-Forwarded-For` entry, which is the load balancer's view of the caller, or the connection's own address without one), `user_agent` and `path`, at most once per address and User-Agent every 10 minutes, and never a token.
  Each field keeps its first 256 bytes, and the server remembers up to 10,000 callers; one past that is logged on each request.
  Callers that share an address and User-Agent share a line, so the log says whether a value on its way out is still in use, not who uses it.

- `DISPATCH_SIGNING_KEY`

  One key, or several separated by whitespace, the first the current key:
  it signs every new session cookie, seals every refresh token Dispatch stores, and, under a key derived from it for that purpose alone, signs the sign-in's state cookie;
  every key of the set verifies a cookie and opens a seal (Identity, below).
  It is parsed as `DISPATCH_AGENT_TOKEN` is:
  whitespace at either end is ignored, and startup refuses an empty or repeated entry, naming its position and never its value.
  A value of nothing but whitespace counts as unset.
  Unset, each process mints a key of its own in its own data directory (`auth.LoadOrCreateSigningKey`), which no other process holds, so a deployment of more than one process gives every process the same keys:
  a session minted on one process is presented to whichever process answers next, and a key that process does not hold signs the person out and leaves their stored refresh token unopenable.
  A process whose key set a live server process does not share both ways, a minted key included, refuses to start, exiting 1 before it serves (Running more than one process, below).
  A rotation in three steps signs nobody out (Identity, below);
  dropping the key a cookie or a seal was made under signs its person out and leaves that seal unopenable.

- `NATS_NKEY_SEED_FILE`, `NATS_NKEY_SEED`

  The NATS nkey user Dispatch connects as: a file holding the seed (trimmed; wins), or the seed.
  A set but unusable value refuses startup naming the variable and path; neither set connects without a credential.
  A NATS that does not answer at boot, or refuses the credential, refuses nothing:
  the server starts and serves, each of its NATS clients connects in the background and retries forever, and a refused credential is logged at ERROR at most once a minute (Running more than one process, below).

- `DISPATCH_TEST_HOOKS`

  Set to `1` to mount test-only routes: `POST /api/v1/live/_test/disconnect` closes every open live socket with 1012 (restart), and `POST /api/v1/live/_test/rotate` closes every one with 4001 (`max age`);
  `POST /api/v1/agents/{session_id}/stream/_test/publish` (human only) feeds the in-memory agent relay, with `as=drop` ending agent views as `dropped`;
  `POST /api/v1/artifacts/_test/quiesce` closes every live document and waits for settlements;
  `POST /api/v1/artifacts/{id}/_test/outside-schema` writes the crafted malformed document e2e uses;
  and `POST /api/v1/writes/_test/slow` `{method, path, delay_ms}` makes every keyed run of that method and exact path wait `delay_ms` once its key is claimed.
  Leave unset in every real deployment.

- `DISPATCH_DEV_SIGNIN`

  Set to `1` to mount `GET /auth/_dev/signin?login=<email>&next=<path>`, which signs any person in by the email it names, with no sign-in pool, so a browser or test harness can be signed in to a local instance.
  Boot refuses it unless identity is `cookie`, no `DISPATCH_SIGNIN_*` setting is set, the listen address is a loopback IP literal, the dashboard origin (`DISPATCH_SERVER_URL` or `dispatch.serverUrl`) names `127.0.0.1`, `[::1]` or `localhost`, every `DATABASE_URL` host is loopback or a unix socket, `DISPATCH_SIGNING_KEY` is unset, `ENVOY_ALLOW_REMOTE_NATS=1` is not set while NATS is on, `DISPATCH_AGENT_SECRETS_URL`, when set, names a loopback host, `ENVOY_URL` names a loopback host, and a loaded GitHub App private key comes from `DISPATCH_APP_PEM_B64` with `DISPATCH_GITHUB_API_BASE` naming a loopback host, never from the `pem` in `app.json`, where a developer keeps the real App's key:
  a signed-in session can have the App probe and import any repository it is installed on, and read GitHub through the App's proxy.
  That key must be a throwaway, as `packages/dispatch/e2e/run-server.sh` generates one, since every App call hands a signed App JWT to whatever listens at that base.
  While it is on every request must carry the dashboard origin as its `Host` (else `421 HOST_MISMATCH`).
  The session cookie is signed with a key generated for that process alone, so it is worthless on any other server, unless `DISPATCH_DEV_SIGNIN_SHARED_KEY` (below) gives the processes of one local harness one key;
  what a signed-in session writes to the database is not.
  It can mint a `dsp_` personal agent token, and its sign-out advances the person's session generation, and every server on the same database honours those rows.
  Give a dev-sign-in server a database no other server uses:
  the loopback check makes that likely, not certain, since a loopback address can be a tunnel to another machine's database or a database a second local server also runs on.
  Without `DISPATCH_DEV_SIGNIN_SHARED_KEY` it generates its key at every start, so a dev-sign-in server refuses to start beside any other live server on its database, and any other server refuses to start beside it (Running more than one process, below);
  a restart after a kill, which leaves the killed process's row behind, waits up to about 20 seconds for that row to lapse.
  The processes given one shared value start beside each other.
  Any value other than `1` or unset is refused.

- `DISPATCH_DEV_SIGNIN_SHARED_KEY`

  Only with `DISPATCH_DEV_SIGNIN=1`, which every fence above still applies to; boot refuses it otherwise.
  Every process given the same value signs session cookies with one key, so a person signed in on one of them stays signed in on the others:
  the browser suite's two-process mode (`packages/dispatch/e2e/run-server.sh`) gives both of its servers one random value per run.
  The key is not the value but the base64url of HMAC-SHA256 keyed by it over the fixed label `dispatch dev sign-in` (`devSignInSharedSigningKey`), so a production `DISPATCH_SIGNING_KEY` placed here signs no cookie, and seals no refresh token, that the production server accepts, and that server's cookies sign nobody in here.
  `DISPATCH_SIGNING_KEY` stays refused beside it.
  Unset, each dev-sign-in process generates a key of its own.

- `DISPATCH_FILE_STORE_BUCKET`

  Files are stored under `files/sha256/<hash>`, one object however often a file is uploaded; a document's markdown never leaves Postgres.
  The credential chain on Fargate is the task role, which needs `s3:GetObject` and `s3:PutObject` on `files/*` and `s3:ListBucket` on the bucket:
  without `ListBucket`, S3 answers a missing object 403, which Dispatch reads as a store failure.
  The SDK must also find a region (`AWS_REGION`, or the shared config); without one the boot is refused.
  An upload writes its object before its row and before any lock or connection is taken, within 30 seconds, so a store that refuses answers 502 `FILE_STORE_UNAVAILABLE` and writes no version.
  The version route serves a row still holding bytes from the row and any other from the bucket:
  it opens the object within 30 seconds, sends its headers and `Content-Length` only then, and streams the body at the client's pace, checking it against the row's hash as it goes;
  a bucket that does not answer is 502, a row whose object the bucket does not hold is 500 `FILE_MISSING` (a restore from the bucket's versioning, not a retry), a cleared row on a server with the setting unset is 503, and a body that fails or hashes wrong mid-stream is cut off rather than ended as if whole.
  `/healthz` reports `files`: null when unset, else whether the bucket answered a two-second `HeadBucket`;
  it never decides `ok`, since a bucket outage must not take the one task, and with it documents and asks, down.
  `envoy-dispatch backfill-files` moves the rows uploaded before the bucket, and `--restore` is the rollback ("Moving uploaded files to the bucket", below).

- `DISPATCH_EMBEDDER`, `DISPATCH_AZURE_EMBED_*`, `DISPATCH_AZURE_TENANT_ID`, `DISPATCH_AZURE_CLIENT_ID`, `DISPATCH_AZURE_FEDERATED_TOKEN_FILE`

  Where meaning search embeds, and with what credential.
  Every process of one deployment takes the same values, and a model change on Azure takes a new deployment name, since each vector records an identity that names the deployment and not the model behind it (Search, below).
  A model service or Entra that is down refuses no boot: search answers by keyword and says so, and the queue retries.

`DISPATCH_REPO_PROJECTS` optionally seeds repository-to-project settings at boot with comma-separated `owner/repo=KEY` entries.
Existing dashboard mappings take precedence over this seed:
it adds only a repository with no mapping, and announces each one it adds with `settings.repo_project.updated`, as a dashboard write does.
Dispatch resolves every external issue through the stored mapping, then falls back to `DISPATCH_DEFAULT_PROJECT` when configured.
An unmapped external repository without a default project is rejected.
An issue created through the default also gets a `repo:owner/name` label.

Where browser sign-in is configured, `DISPATCH_SERVER_URL` IS the sign-in callback origin:
it must equal the URL humans type into their browser, and the sign-in pool's app client must list `<DISPATCH_SERVER_URL>/auth/callback` as a callback URL.
With no `DISPATCH_SIGNIN_*` setting nobody signs in through a browser, and the variable is the origin Dispatch builds its own links from and nothing more.

`DISPATCH_ALLOWED_LOGINS`, `DISPATCH_APP_CLIENT_SECRET` and `DISPATCH_ASSET_STORE_BUCKET` are removed:
a boot that still sets any of them is refused, naming what replaced it.

Agents normally authenticate with a personal `dsp_` token minted in Settings, sent as `Authorization: Bearer <token>`.
`DISPATCH_AGENT_TOKEN` remains the shared devbox fallback and does not attribute callers to an owner.

With `DISPATCH_OIDC_ISSUER` and `DISPATCH_OIDC_AUDIENCE` set, a Kubernetes pod's projected service-account token is a third bearer credential:
the server reads the issuer's discovery document in the background from boot, retrying after one second and doubling to thirty while it fails (each failure logged at ERROR at most once a minute), and an issuer that cannot be reached refuses nothing;
it verifies a JWT-shaped bearer against the issuer's published keys, the configured audience, and the token's expiry.
Until the document has been read, a JWT-shaped bearer is `503 OIDC_ISSUER_UNAVAILABLE`, never a 401, and every other credential is served.
Half the pair refuses to boot, naming the missing variable; neither leaves bearer handling exactly as it is without them.
A verified caller acts as the `session` it names in the request body, and every actor it writes carries `service`, the token's verified subject (`system:serviceaccount:<namespace>:<name>`)
— set from the token, never from the body.
A JWT the verifier rejects is `401 OIDC_TOKEN_INVALID` naming the reason class; it is never retried as a personal token.

With `DISPATCH_AGENT_SECRETS_URL` set, an access token the secrets broker issued is a fifth credential, and the only one that proves a session of its own.
It is presented under RFC 9449's DPoP scheme
— `Authorization: DPoP <token>` with a `DPoP:` proof header signed by the key the token is bound to
— because that RFC refuses a DPoP-bound token presented under any other scheme;
the server decides a request by its scheme before it reads any token, so such a presentation is never retried as a bearer, and a bearer is never sent to this check.
The server asks the broker what the token is (RFC 7662 introspection on `/v1/oauth/introspect`, presenting the same projected ServiceAccount token `DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE` names as the approval relays do, one affirmative answer reused for five seconds from the broker's answer) and checks the proof against the `jkt` that answer carries, with `DISPATCH_SERVER_URL` as the origin the proof's `htu` must name.
No setting of its own configures it.
A revoked token therefore stops resolving within five seconds of the introspection that last called it live, not of the revocation:
an introspection already in flight when the token is revoked is remembered from the moment it completes, and it may take up to five seconds to complete.
A verified caller acts as the session the token names, for the person it names, and `actor` in the body does not decide that session:
absent is accepted, one naming the same session is accepted and its `origin` kept, and one naming any other writer is `403 ACTOR_SESSION_MISMATCH` naming the session the token carries.
A token scoped to one project carries that project on every actor it writes, set from the token and never from the body;
a token scoped to its person names no single project and carries none.
Each refusal is its own code and none repeats the token:
`401 BROKER_TOKEN_NOT_ACTIVE` (the broker does not hold the token live), `401 BROKER_TOKEN_PROOF_INVALID` (the proof did not meet RFC 9449 Section 4.3, a proof presented twice included
— a proof is single use by its `jti` and target URI), `401 BROKER_TOKEN_KEY_MISMATCH` (a sound proof signed by a key that is not the token's), `401 BROKER_TOKEN_NOT_A_SESSION` (the token is live but is no session's credential:
it names no session or no person, or it was issued to a client other than the exchange a session's token comes from, which a machine login's own access token is
— Dispatch records a write under the session its credential proved, so it acts on none of these rather than letting a body name a session for a credential that names none), `429 BROKER_TOKEN_RATE_LIMITED` (this source address's introspection bucket is empty;
nothing was asked of the broker) and `503 BROKER_UNAVAILABLE` (the broker answered nothing, so the token was never judged
— a 503 for the reason `OIDC_ISSUER_UNAVAILABLE` is one).
A websocket re-resolves its caller every heartbeat from the request that opened it, and for this credential that re-resolution is the broker's introspection answer alone:
the upgrade's proof authenticated it and cannot be presented a second time.
Without `DISPATCH_AGENT_SECRETS_URL` there is no such credential, and a `DPoP` presentation is the `401 UNAUTHORIZED` `invalid bearer token` any Authorization header that is not a bearer has always been.

A credential that names one project may write that project and nothing else.
Every mutating row of the route table declares what its write belongs to (`internal/dispatch/api/routes_table.go`, `writeClasses`;
a `GET` row declares nothing), in four classes:
*a project's* — the issue-key rows, the project-key rows, the artifact-id rows, the ask-id rows, the comment-id rows, and the message rows that carry an issue;
*a project the body names* — the issue create alone, whose project its address does not name, so `createIssue` makes the same check itself once that project is settled;
*the calling session's* — an agent conversation's own artifact and messages, the two routes a session answers its own delivery on, and the caller-private `me` rows;
and *the deployment's* — the project create, the repository-to-project map, the delivery settings and session groups, the broadcast, and the credential and machine-login decisions.
`Register` wraps every mutating route in two checks (`internal/dispatch/api/project_scope.go`), which run after the caller is resolved and before the handler, so no route is left to check either for itself.

The project check resolves the project the addressed object belongs to
— an issue key, a project key, an artifact id, an ask id, a comment id or a message id, each by one read
— and lets through only a target that resolves to the caller's own project.
It fails closed, so a target that resolves to another project, to no project at all (an object belonging to none), or to nothing at all (no such row, or an id Postgres will not read as a uuid) is `403 PROJECT_SCOPE`, naming the project the credential covers and the project the write belongs to where one resolved, and nothing of the credential.
The two refusals still read differently — a target of another project names that project, one that resolved to nothing says the write resolves to no project of that credential's
— so failing closed does not close an existence oracle and does not claim to.
Nothing is read out that way: no GET route is project-scoped, so every answer this check can colour is a refusal of the caller's own write.
Each resolver sends the raw path value to Postgres, as the handler behind it does, so the check reads a path exactly as the handler then addresses its row by it
— Postgres's uuid parser takes a hyphen after any group of four digits, and a check that parsed ids in Go admitted those spellings unjudged.
A deployment-wide write is `403 PROJECT_SCOPE_DEPLOYMENT`, naming the credential's project and the route:
a credential that may write one project may not create another or change a setting that is not any project's.
`POST /api/v1/issues` takes its project from the body, or from the repository an external ref names, rather than from its address, so its handler makes the same check, with the same refusal, the moment that project is settled
— before the ref is resolved to an issue already imported, and before anything is written.

The session check holds the rows classed *the calling session's* whose path names a session
— the `agents/{session_id}` writes and `PUT /api/v1/me/agents/{session_id}/state` — to the session the caller's credential proved:
addressing another session's conversation is `403 SESSION_SCOPE`, naming the session the credential acts for, the route and the session addressed.
Only a broker-issued token proves a session;
a cookie, a personal token, a Kubernetes service-account token and the shared token prove none and are untouched, so a person still uploads into the conversation they name.

Both checks are a no-op for a credential that carries no project and proves no session
— a browser cookie, a personal agent token, a Kubernetes service-account token, the shared token, and an access token the broker scoped to a person rather than to one project.
That is the rule and not a transitional measure: every caller that exists today writes exactly what it wrote before.

GitHub App credentials come either from the `DISPATCH_APP_*` variables or from `~/.local/share/dispatch/app.json`; the variables take precedence.
Two of them carry rules beyond their row:

Each item below names a variable, then its purpose.

- `DISPATCH_APP_PEM_B64`

  GitHub App private key, as a PEM or the base64 of a PEM: a value holding `-----BEGIN` is the PEM itself, and any other is base64-decoded.
  A key that does not parse stops boot with an error naming the variable, never the key.
  With `DISPATCH_DEV_SIGNIN=1` this is the only source a key may come from (a `pem` in `app.json` is refused), it must be a throwaway, and `DISPATCH_GITHUB_API_BASE` must name a loopback host.

- `DISPATCH_GITHUB_API_BASE`

  GitHub API origin override for App calls (tests and e2e point it at a fake); empty means `https://api.github.com`.
  With `DISPATCH_DEV_SIGNIN=1` and an App private key loaded, it must name `127.0.0.1`, `[::1]` or `localhost`.
  That checks the host, not what listens there: every App call hands a signed App JWT to whatever owns the port, so the key must be a throwaway.

When no GitHub App credentials are configured, the server still starts, but the GitHub proxy answers `503 GITHUB_TOKEN_UNAVAILABLE`, and saving a project's architecture source answers `409 SOURCE_ACCESS` naming the missing key.

For a Google Cloud deployment, use `envoy-infra:gcp:Dispatch` from `packages/envoy/infra`.
It supplies the private Cloud SQL connection, the tailnet entry point, and the pod environment this binary needs;
see that component's README for its inputs, the IAM grant it writes and the one the consumer writes.

## Webhook redelivery

GitHub does not redeliver a webhook delivery that failed.
When NATS is configured and the App's private key (`DISPATCH_APP_PEM_B64`) is present, Dispatch redelivers them itself, following GitHub's documented approach.
At start and every two minutes after (Background loops, below:
each sweep two minutes plus up to a tenth after the last) it lists the App webhook's attempts (`GET /app/hook/deliveries`) and judges each delivery by its GUID, as GitHub's own redelivery script does:
a GUID with an `OK` attempt is delivered, whatever its failures, and it asks GitHub to redeliver each other GUID with an attempt whose status is not `OK` (`POST /app/hook/deliveries/{id}/attempts`).
Both endpoints accept only an App JWT, which is why Dispatch runs this.
The webhook the App delivers to is the Envoy listener's `/webhook/github`.
A redelivery carries the original `X-GitHub-Delivery`, and the listener publishes GitHub envelopes under a JetStream MsgId of it.
So redelivering a delivery that did reach the stream adds nothing to the stream.

- Every failed delivery is asked for again, whatever status code its failed attempts carry:
  a 5xx, an attempt GitHub could not complete, and a 4xx alike (the listener's 401 for a signature it cannot check, 400 for a body it cannot use, 422 for a publish NATS refused), since what the listener cannot take now it may take later.
  The first request goes on the next sweep.
  While no attempt is `OK`, the next request is due 2, 4, 8, 16 and 32 minutes after the one before, then every 60 minutes.
  A redelivery request GitHub refuses (for example 422) delivers nothing and is asked again on the same schedule, as is an accepted request whose redelivery never appears in GitHub's log.

- The sweep stops asking 72 hours after the first failed attempt it saw for the delivery, since GitHub keeps a delivery 72 hours, or when GitHub answers the redelivery request 404 (it no longer has the delivery).
  It logs `level=ERROR msg="webhook delivery expired undelivered"` once per delivery, with `requests`, `first_delivered_at` and `reason` (`GitHub keeps a delivery 72 hours` or `GitHub no longer has the delivery`).

- A rate-limited answer (403 or 429, as GitHub's REST rate-limit documentation describes one) stops the sweep and counts nothing against the delivery.
  No sweep sends GitHub anything until the time GitHub gave (`Retry-After`, or `x-ratelimit-reset` when no requests remain), and at least one minute, doubling while limits follow one another.
  Each sweep reads the recorded limit again right before every request it sends, so one limit stops every sweeper at its next request;
  a request already sent when another sweeper records a limit is not recalled.
  It logs `level=WARN msg="webhook redelivery rate-limited by GitHub"`.
  Redelivery requests go a second apart, as GitHub asks of a large number of POSTs.

- A delivery GitHub has recorded an `OK` attempt for is never asked for again, even when a request for it was recorded as refused (an accepted request whose answer was lost).
  When the sweep had made at least one request for it, it logs `level=INFO msg="webhook delivery recovered"` once, with `requests`, the number of requests the delivery took.

- Every sweep logs an INFO `msg="webhook redelivery sweep"` line:
  `failed_attempts` (the failed attempts listed), `deliveries` (the deliveries the sweep decided:
  the GUIDs of the listed failed attempts, plus the open deliveries the listing no longer returns), and one count per outcome, which add up to `deliveries`.
  `closed` is a delivery an earlier sweep settled (GitHub recorded it `OK`, or it expired):
  its failed attempt stays listed until it is older than the sweep's hour, so after a burst of failures is redelivered, the next hour's sweeps count the burst as `closed`.
  `waiting` is a delivery whose next request is not due yet.
  A delivery the sweep's cap of 300 requests left over is not counted; the next sweep takes it.

- A sweep looks back one hour, and decides every delivery whose record is still open, neither delivered nor expired, even when its attempts are older than that.
  After a gap in sweeping (Dispatch down), it resumes from its cursor, back to GitHub's three days.
  The first sweep ever made, with no cursor, looks back 72 hours.

The cursor, the last rate limit, and one record per delivery acted on live in the JetStream KV bucket `envoy_webhook_redelivery`.
Its 72-hour TTL matches GitHub's horizon.
The server opens the bucket on the first sweep that finds NATS connected, not at boot, so a NATS that is down at boot skips the sweeps until it answers, each logged as failed.
A compare-and-swap claim keeps two Dispatch processes from requesting the same attempt twice, and the operator command honours the same rate limit.

The operator command runs the same sweep over a chosen window.
It does not move the running sweep's cursor.
`--dry-run` lists every failed delivery and what the sweep would do with it, and requests and writes nothing:

```bash
envoy-dispatch redeliver-webhooks --since 72h --dry-run   # list what would be redelivered
envoy-dispatch redeliver-webhooks --since 72h             # redeliver under the sweep's rules
```

It reads the same App credentials and NATS configuration as the server, but owns nothing on the bus:
unlike the server it neither creates nor updates `ENVOY_NOTIFICATIONS`, and it refuses a NATS server that is not the machine it runs on, naming the URL.
It opens the redelivery bucket as the server does, at `ENVOY_NATS_REPLICAS` copies (creating it, or raising one it finds with fewer), so run it with the server's value;
a value other than a whole number from 1 to 5 stops it before it connects.
It runs no keeper: the server's keeper raises the bucket back to the server's value every 30 seconds, and a refused raise names the bucket, `raise KV_envoy_webhook_redelivery: ENVOY_NATS_REPLICAS=3: context deadline exceeded` under a grant without its stream update.
Run inside the Dispatch container it reaches that deployment's NATS with `ENVOY_ALLOW_REMOTE_NATS=1
envoy-dispatch redeliver-webhooks …`.
It connects as the nkey user `NATS_NKEY_SEED_FILE` (or `NATS_NKEY_SEED`) names and runs no `!` command, so it never reads `envoy.natsSeed`:
a run against a NATS that requires a user needs `NATS_NKEY_SEED_FILE`.

## Moving uploaded files to the bucket

With `DISPATCH_FILE_STORE_BUCKET` set, new uploads go to the bucket;
the files uploaded before it stay in their rows until `backfill-files` moves them, oldest first, one row at a time:
it writes the object under the row's hash (a write S3 verifies against that hash, and the store refuses before any call when the bytes do not hash to it), reads it back and checks it, and only then clears the row's bytes, in one statement that clears only a row still holding them.
A row it cannot move (a store failure, bytes that do not hash to the row's hash, a row with no hash) is printed as `FAILED version <id>` and passed over, so one bad row holds up no other;
the run exits 1 when any row failed, and can be run again at any time, since each row is done or untouched and the server serves a row until its bytes are cleared.
Two writing passes at once (`backfill-files` and `--restore`, in any pairing) are refused by an advisory lock (`another backfill-files pass holds the
lock`).
`--verify-only` reads back every cleared row against its hash, moves nothing, takes no lock (it runs beside either pass, reading only what that pass has finished), and exits 1 naming each row whose object is missing or wrong.

`--restore` is the rollback, at any point before or after the clear:
it reads every cleared row's object back, verified against the row's hash, and writes the bytes into the row again, so a server with the setting unset, or an image from before the store existed, serves every file from its row.
The objects stay in the bucket, and a later `backfill-files` moves the rows out once more.
Unsetting the setting without a restore is not a rollback:
a server without it serves a row still holding bytes and answers `503 FILE_STORE_UNAVAILABLE` for every row written while the bucket was set, an upload's as much as a cleared one's;
and an image from before the store serves a cleared row as an empty `200` under the file's own `ETag`, which a browser then caches.
Restore first, then roll back.

```bash
envoy-dispatch backfill-files                 # move every row still holding bytes; prints each one and a total; exit 1 if any failed
envoy-dispatch backfill-files --verify-only   # read back every moved row; exit 1 naming any that fails
envoy-dispatch backfill-files --restore       # the rollback: write every moved row's bytes back from the bucket
```

It needs `DATABASE_URL` and `DISPATCH_FILE_STORE_BUCKET`, refuses a bucket it cannot `HeadBucket` before touching a row, and reads nothing else of the server's configuration.
Clearing a row's bytes leaves dead tuples behind:
the table shrinks on disk only after `VACUUM FULL` or `pg_repack`, while the nightly dump shrinks at once; measure the dump, or vacuum first.

To run a local server against a bucket of your own, point the SDK at an S3-compatible server with `AWS_ENDPOINT_URL_S3=http://127.0.0.1:<port>` (an IP literal:
with `localhost` the SDK addresses the bucket as a subdomain, which does not resolve), `AWS_REGION`, static `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, and `AWS_EC2_METADATA_DISABLED=true`.
The Go tests start one (`internal/tests3`, SeaweedFS in a container, since MinIO's image left Docker Hub) and create the bucket with an anonymous `PUT /<bucket>`.

## Identity

`DISPATCH_IDENTITY` controls how browser requests identify a person, always by lowercase email:

- `cookie` (the default) accepts signed `dsession` cookies, which a sign-in issues (OpenID Connect authorization code), through one of two issuers.
  Beside `DISPATCH_SIGNIN_ISSUER`, `DISPATCH_SIGNIN_CLIENT_ID` and `DISPATCH_SIGNIN_CLIENT_SECRET`, exactly one of `DISPATCH_SIGNIN_GROUP` and `DISPATCH_SIGNIN_DOMAIN` is set;
  both, some without the others, a domain under any issuer but `https://accounts.google.com`, a group under that one, or a domain that is not a lowercase DNS name refuse the boot, each naming what is wrong (`checkSignInSettings`).

  With no `DISPATCH_SIGNIN_*` setting at all the server boots and runs no browser sign-in:
  it issues no `dsession` cookie, and it refuses every one it has issued, answering each request that needs a person `401 NO_IDENTITY` (`requestIdentityFor`).
  A cookie minted while sign-in was on still verifies for its thirty days, since the key signing it is the deployment's `DISPATCH_SIGNING_KEY` and outlives the configuration, and with no issuer nothing is left to confirm that the person it names may still use Dispatch.
  The agent-token API is served as it always is, and `GET /auth/start` answers `503 SIGNIN_UNCONFIGURED`.
  That is what a sandbox deployment runs (`envoy-infra:gcp:Dispatch` with its tailnet inputs unset, which has no origin an OAuth provider takes).
  A partial set is still half a sign-in and refuses the boot, naming every setting it misses (`checkSignInSettings`).
  Dev sign-in (`DISPATCH_DEV_SIGNIN`, below) sets none of these either and keeps its cookies: it mints them itself.

  The issuer's discovery document is read in the background from boot, on the service-account issuer's schedule (above), and an issuer that cannot be reached refuses nothing:
  until the document has been read, `/auth/start` and the callback answer `503 SIGNIN_UNAVAILABLE`, and the rest of Dispatch serves.
  A sign-in in flight (its state, its nonce, the page it returns to and when it lapses, ten minutes after it started) travels in the browser's `dispatch_oauth_state` cookie, signed under a key derived from the current signing key for that purpose alone, and no process keeps it:
  a sign-in one process started completes on whichever process the issuer's redirect reaches, given one `DISPATCH_SERVER_URL` on every process:
  the exchange must send the callback URL the sign-in started with, and each process builds it from that setting, or without one from the request's `Host`.
  The callback clears the cookie first, and answers `400` "invalid or expired state" to a cookie that does not verify under a key of the set, has lapsed, or names a state other than the one the issuer returned.
  Each sign-in completes at most once: the issuer's code is single-use, and the exchange accepts only an ID token carrying the cookie's nonce.

- Through a shared Cognito sign-in pool (`DISPATCH_SIGNIN_GROUP`), the callback names the person by the email in their pool username (`<provider>_<email>`, for a provider the ID token's `identities` names), never by the `email` claim, which a person can write, and signs in only a member of `DISPATCH_SIGNIN_GROUP`;
  anyone else gets a 403 page naming them.

  With Google directly (`DISPATCH_SIGNIN_DOMAIN`, under Google's issuer and a Google web client whose redirect URI is `<DISPATCH_SERVER_URL>/auth/callback`), the authorization request asks for offline access with `prompt=consent`, so every exchange returns a refresh token, and names the domain as `hd`, a hint only.
  The callback signs in a person whose ID token's `hd` is the domain and whose `email_verified` is true (the JSON `true` or the string `"true"`;
  any other value is false), named by that `email`, lowercased; the token's `hd` decides, never the email's own domain.
  Anyone else gets a 403 page with the reason: not in the domain, email not verified, or no email.
  The code flow reads those claims from the verified token in domain mode alone;
  the shared verifier, which Cognito tokens, pods' tokens and the listener go through, reads none of them.
  A refusal logs `msg="dispatch: sign-in refused" email=<email> hd=<hd> reason=<reason>`, where a pool refusal logs `username=<username>`.

  Either way, Dispatch keeps the issuer's refresh token and confirms the person with it at least hourly, the hour measured on the database's clock (`people.confirmed_at`), so processes whose clocks differ agree on when it lapses.
  A refresh the issuer refuses (`400` or `401` with the OAuth error `invalid_grant`:
  the token expired, was revoked or is not the client's), or one whose ID token no longer passes the rule or names another email, ends every session they hold.
  A refresh that fails any other way (the issuer undiscovered, down or slow, a `5xx` or `429`, another OAuth error, or an answer without an ID token that verifies) ends nothing:
  the request that found the confirmation lapsed is refused `503 SIGNIN_UNAVAILABLE`, the sessions and the refresh token are kept, and the first request after the issuer answers again refreshes and is served.
  Logout ends Dispatch's session only, not the issuer's.

- A person is their email, so an address passed from one account to another is one person.
  In domain mode `people.sub` records the Google account (`sub`, never reused) that last signed in to the address:
  a sign-in under another `sub` ends every session of the address in the transaction that records the new one (a failed revoke records nothing, so the retry revokes again), logged `msg="dispatch: a sign-in under another Google account ended every session of the address"` at WARN with the email, and the hourly confirmation ends them when the refreshed token's `sub` is not the recorded one.
  A sign-in whose address another account signs in to between its record and its cookie gets `409 SIGNIN_SUPERSEDED` and no cookie:
  after reading the session generation the callback checks that the row still records its `sub`.
  Group mode neither reads nor writes `sub`, so there the old holder of a reassigned address keeps the cookies they have until their cookie expires, 30 days after it was issued, unless the pool user is removed from `DISPATCH_SIGNIN_GROUP` or disabled.

  The refresh token is stored sealed (`people.refresh_token`):
  AES-256-GCM under a key derived from the current key of `DISPATCH_SIGNING_KEY` with HKDF and a fixed purpose label, bound to the person's email, in a format whose `v1:` prefix names its version.
  A value is opened under each key of the set in turn, the GCM tag refusing a key it was not sealed under;
  it names no key, so the `v1:` format is byte for byte the one earlier releases write and read.
  A stored value that does not open (sealed under a key the set does not hold, moved from another person's row, or in plain text) counts as no refresh token:
  each read of it logs `msg="dispatch: a stored refresh token did not open; the person signs in again"` at WARN with the email and never the value, and writes nothing;
  the person's request answers `401 NO_IDENTITY`, and their next sign-in replaces the value.

- A refresh token stored in plain text (by the release before sealing, or by a task of it still serving during a roll or after a rollback) can be redeemed by anyone holding a copy of the row and the app client's secret until it expires, and forgetting it does not end it, so Dispatch retires it:
  at every boot, after migrations, it revokes each plain-text token at the pool's revocation endpoint (the `revocation_endpoint` the issuer's discovery document names) as the app client, then clears it and its confirmation while the row still holds that value.
  A sign-in that replaces a plain-text token retires it the same way first.
  A sealed value is never sent to the pool, and a redirect from the revocation endpoint is a failure, never followed.
  A token the boot cannot revoke, the issuer's discovery document not yet read included, stays in its row for the next boot and logs `msg="dispatch: the sign-in pool did not revoke a refresh token stored in plain text"` at ERROR with the email and the error (the pool's HTTP status, when it answered), never the token.
  A sign-in that cannot revoke the token it replaces goes on, since the person just signed in at the pool, and logs `msg="dispatch: a sign-in replaced a refresh token the pool did not revoke; sign this person out at the pool"` at ERROR with the email and the error:
  no later boot will see that token, so signing the person out at the pool is the only way to end it.
  The boot's retirement stops after 30 seconds, failing each token it has not revoked, and the start goes on;
  it logs `msg="dispatch: retired the refresh tokens stored in plain text" retired=<n> failed=<n>`.

  A database backup therefore holds no refresh token the pool would accept only once a boot of this release has retired every plain-text token, and then only for the tokens still in `people` when it ran:
  those stop working when the pool revokes them.
  A plain-text token `0068`'s release removed without revoking (a sign-in that replaced it, or a logout that cleared it) stays valid at the pool until it expires, in any backup that holds it (Database migrations, below).

  A rotation of `DISPATCH_SIGNING_KEY` signs nobody out when it is made in three steps, each rolled to every process before the next:
  (a) add the new key second, after the current one; (b) move it first; (c) remove the old key, no sooner than 30 days after (b), the session cookie's lifetime.
  A cookie made under the old key verifies until it expires, and a refresh token sealed under it is sealed again under the new key by the person's next hourly confirmation.
  Each step passes the boot's key check beside processes still on the step before (Running more than one process, below).
  Replacing the key in one step fails that check beside a live process that holds only the old key, and on a lone process signs everyone out.
  The release that first ships the key set is never also the release of a first rotation, and no build older than the key set is rolled back to while the setting holds two values:
  such a build reads `"K1 K0"` as one key, which signs everyone out.
  Removing a key revokes no refresh token at the pool:
  whoever holds a backup and the old key can still redeem the tokens sealed under it until they expire, so after a key leak, sign each person out at the pool.

- `header:<Header-Name>` is for tests and local harnesses only, never for a production Dispatch deployment.
  It accepts a header value, lowercases it and records the person.
  The harness must set `DISPATCH_IDENTITY_HEADER_TRUSTED=1`;
  Dispatch refuses header identity when that flag is absent or when any `DISPATCH_SIGNIN_*` setting is present, so it cannot share a deployment with Google sign-in.

The people Dispatch has seen, either way, are the assignee picker's options and the only names an issue may be assigned to.

`DISPATCH_INSECURE_COOKIE=1` omits the `Secure` attribute for local plain-HTTP testing.
Do not use it on an HTTPS deployment.

`DISPATCH_DEV_SIGNIN=1` adds a second way to get the cookie on a local instance:
`GET /auth/_dev/signin?login=<email>` records the lowercased person and issues the session cookie an issuer sign-in does, with no issuer exchange and no group check.
The route serves only a loopback peer whose request carries no forwarding header, logs every mint at WARN, and boots only behind the fence its Required configuration item lists.
A request that reaches the process looking local (`ssh -L`, `socat`, a proxy that rewrites `Host` and adds nothing) is indistinguishable from one that is.
The per-process key bounds the cookie, not every credential a dev session can write:
a `dsp_` token it mints, and the session and token rows its sign-out changes, live in the database, and every server that reads that database acts on them.
A dev-sign-in server therefore needs a database of its own.
The one exception to the per-process key is a harness that runs several dev-sign-in processes on one database on purpose, as the browser suite's two-process mode does:
`DISPATCH_DEV_SIGNIN_SHARED_KEY` gives them one key, derived from the value under a dev-only label, so their cookies are each other's and never a production server's.

## Local Postgres

Start the local database once:

```sh
cd packages/envoy
./scripts/dev-postgres.sh
```

It prints the `DATABASE_URL` to use for development and tests.
Run again, it starts or reuses the same `dispatch-pg` container, which runs its pinned image:
pgvector on PostgreSQL 18, whose `vector` extension the migrations need.
It refuses a `dispatch-pg` on any other image or major and prints the commands that dump that container's databases and restore them into a new container on the pinned image.
The database retains the migrated schema; tests only truncate rows they create.


## Running locally

Run `bun run dispatch:local` at the repository root.
It builds this server from the tree, runs it in cookie identity with the dev sign-in route, on a seeded database, and prints the URL that opens the dashboard signed in;
`packages/dispatch/README.md` ("Local instance") documents it.

The default listen address is `:8766`.
Set `DISPATCH_LISTEN_HOST` and `DISPATCH_PORT` to change it; `DISPATCH_LISTEN_HOST` may be an IPv6 literal with or without brackets (`::1` or `[::1]`).
A bad `DISPATCH_PORT` refuses the boot before anything connects, and an address the server cannot bind ends the process with exit status 1.

A test harness that holds the port instead hands the server a listening TCP socket it opened on port 0, so no other program can take the port the kernel chose before the server serves on it:
`DISPATCH_LISTEN_FD` is the descriptor number the server inherits it as.
The server then binds nothing.
It serves on that socket, and `DISPATCH_LISTEN_HOST` and `DISPATCH_PORT`, when set and not empty, must name the socket's address (`DISPATCH_PORT=0` never does).
A descriptor that is not a listening TCP socket, or a setting that names another address, refuses the boot, and nothing is bound in its place.
`DISPATCH_SERVER_URL` is not derived from the socket.
Either way the server logs the address it serves on once it serves, as `dispatch: listening addr=<host:port>`.

### Profiling

Set `DISPATCH_PPROF_PORT` to a port to serve the Go profiler (`net/http/pprof`) on `127.0.0.1:<port>`, on a mux of its own, apart from the server's routes.
It binds the loopback address only, so nothing outside the pod reaches it; unset or empty, nothing listens.
A value that is not a port 1-65535, or a port already taken, refuses startup naming the variable.
The profiler binds before anything connects, so a slow boot can be profiled too.
The listener closes at once when the process is told to stop, cutting off a profile in flight, so it never delays the shutdown (and the shutdown's own drain cannot be profiled).
A `gcp:Dispatch` deployment turns it on with the component's `pprofPort` input (`packages/envoy/infra/README.md`), which sets the variable on the `envoy-dispatch` container;
setting it by hand on the Deployment rolls the pods and is reverted by the next update.
To take a 30-second CPU profile of a pod:

```sh
kubectl port-forward <pod> <port>:<port>
go tool pprof -seconds 30 http://127.0.0.1:<port>/debug/pprof/profile
```

`/debug/pprof/` lists the other profiles (`heap`, `goroutine`, `allocs`, `threadcreate`, `trace`).
`mutex` and `block` are listed too but record nothing: Dispatch sets no mutex or block sampling rate.

## The live socket behind a proxy

A page's live updates ride one WebSocket per tab, `GET /ws/live`, over HTTP/1.1.
A browser pools WebSockets apart from its six HTTP connections per host, so a socket that stays open for its whole age never holds one of the slots ordinary requests need (`docs/solutions/dispatch-web/a-never-ending-http-request-from-a-browser-page-pins-chromes-six-connections-once-its-cert-verifier-updates-INFRA-258.md`).
That holds only while the front proxy the browser reaches (the load balancer or ingress) does not advertise HTTP/2 extended CONNECT (RFC 8441, `SETTINGS_ENABLE_CONNECT_PROTOCOL`).
A browser that sees it runs the live socket as a stream of its HTTP/2 session instead, a session the browser marks going away stays open while that stream lasts, and the INFRA-258 hang can return.
`bun scripts/e2e/lib/h2-settings.ts https://<dispatch>` checks it:
it prints the HTTP/2 SETTINGS the proxy sends as one JSON line, and exits 1 when they advertise extended CONNECT, 0 when they do not or the proxy negotiates no HTTP/2, and 2 when it gets no answer (`scripts/e2e/README.md`).
Run it after any change to the proxy.

## Checking a deploy

After a deploy, check through the front proxy that the live socket, the keyed write path and the two keyed sends work.
Run the commands from a checkout of this repository with `bun install` run, each against `https://<dispatch>`, the deployment's dashboard origin, and the two sends from a browser signed in to it.
`<token file>` holds an agent token (the shared one, or a personal `dsp_` token from Settings → Agent tokens), and `<scratch issue>` is the key of an issue the checks may leave comments on.

1. `bun scripts/e2e/lib/h2-settings.ts https://<dispatch>/` exits 0: the proxy does not advertise extended CONNECT (the section above).

2. `curl -s https://<dispatch>/healthz` prints a body with `"idempotency_keys":true`: this build judges `Idempotency-Key` (Keyed writes, below).
   The field is a fact of the build, so a `503` for a database or NATS that is down carries it too;
   a body without it is a Dispatch that would make a retried write twice, and the `dispatch` command sends no write again to it.

3. `bun scripts/e2e/lib/dispatch-keyed-comment.ts --url https://<dispatch> --bearer-file <token file> --issue <scratch issue>` posts one comment twice under one `Idempotency-Key`, as a page whose first answer was lost asks again.
   It passes when the repeat is Dispatch's stored answer and the issue holds the comment once:
   it prints `{"post":2,…,"replayed":"?1","status":201}`, `{"comments_with_body":1}` and `{"ok":true}`, and exits 0 (`scripts/e2e/README.md` lists every line and failure).

4. The same command with `--cli` has this checkout's `dispatch comment` post it, through a proxy on `127.0.0.1` that hands the command a gateway's `502` page in place of Dispatch's answer to its first POST, so the command asks again under its own key.
   It passes when it prints two `proxy` lines for that POST under one `key`, the first `"answered":502` and the second `"replayed":"?1"`, then `{"cli":{"code":0,…,"stdout":"Posted comment <id>"}}`, `{"comments_with_body":1}` and `{"ok":true}`, and exits 0.

5. A page opened while a write commits shows it (INFRA-292).
   Save the `Cookie` request header a browser signed in to the deployment sends (the developer tools' Network panel, any request to `https://<dispatch>`) to `<cookie file>`, and run `bun scripts/e2e/lib/dispatch-live-events.ts --url https://<dispatch> --cookie-file <cookie file> --from-whoami --subscribe-after 30 --until <marker> --timeout 120`.
   It reads `GET /auth/whoami` as you, prints `{"whoami":{"event_head":"<id>","login":"<you>"}}` and `{"subscribing_in":30}`, and only then opens `/ws/live` from that `event_head`, as a page resumes its first subscription.
   Inside those 30 seconds, comment on `<scratch issue>` with `<marker>` in the body: the comment commits after whoami answered and before the socket opens.
   It passes when it prints the comment's `{"event": …}` line and exits 0; it exits 1 when the 120 seconds end first.
   Delete `<cookie file>` afterwards: it signs in as you.

6. `bun scripts/e2e/lib/dispatch-live-events.ts --url https://<dispatch> --bearer-file <token file> --until <marker> --timeout 4200` holds one `events` subscription on `GET /ws/live` for up to 70 minutes, longer than the 30 to 60 minutes a socket lives, under the bearer the file holds.
   It prints one JSON line per frame: `{"event": …}` for each event, `{"close": <code>, "reason": "…"}` when the socket closes, `{"resubscribed": "<id>"}` when it opens the next socket and resumes after the newest event it holds, and `{"duplicate": "<id>"}` for an event it already printed.
   Once a `{"close":4001,"reason":"max age"}` line and the `resubscribed` line after it have printed, comment on any issue with `<marker>` in the body:
   the probe exits 0 when that event arrives on the new socket, and 1 when the 70 minutes end first (2 is a usage error).

7. A send across a reload: on the Agents page, open a live agent's row, set the browser offline (the developer tools' Network panel), send a message, and reload the tab, confirming the browser's prompt to leave.
   Within five minutes set the browser online and open the Agents page again in the same tab.
   The message lands: the row shows it once and the agent receives it once.

8. A broadcast across an outage: tick two live agents, set the browser offline and press Send.
   After 30 seconds the send's row reads `Still sending to 2 agents — Dispatch has not confirmed it
   yet. It keeps trying, and the broadcast is made once.` with **Stop trying**.
   Set the browser online: the page opens the broadcast, which lists each agent once with one delivery.

## Running more than one process

Two `envoy-dispatch` processes serve one Postgres and one NATS, so a release rolls instead of taking Dispatch away from everyone while it deploys.
A rolling update is supported, under two preconditions; the server checks the first itself.

**Every process is given the same `DISPATCH_SIGNING_KEY`.**
A session cookie, or the sign-in's state cookie, minted by one process is presented to whichever process the next request reaches, and every stored refresh token is sealed under a key derived from the current key (Identity, above).
Unset, each process mints a key of its own in its own data directory (`cmd/dispatch/main.go`, `auth.LoadOrCreateSigningKey`), which no other process holds.
The server refuses that through the registry of live server processes (`internal/dispatch/processes`, migration `0135`):
once a process has bound its port, and before it serves, it registers the fingerprint of each of its keys (none reveals its key), and it refuses to start, logging `msg="dispatch: register this server process"` at ERROR with an error that names the other process and the rotation steps, and exiting 1, when a live process's key set does not share its own both ways:
the other's current key among its keys, and its current key among the other's.
The refusal stands only beside a process that stays live:
refused, a process logs at WARN which live process refuses it and waits up to one live window (15 seconds) and a heartbeat for that process to stop heartbeating, trying again every heartbeat, before it exits 1, serving nothing meanwhile (a SIGTERM or Ctrl-C during the wait is a clean stop:
the process logs `msg="dispatch: stopped while
waiting for admission to the registry of live server processes"` at INFO and exits 0).
So a restart after a kill, whose dead process's row still counts as live, starts once that row lapses, while a live process, heartbeating every 5 seconds, keeps refusing.
A process with a minted key therefore refuses beside any live process, and a process refuses beside it;
processes that share one data directory share its key and start.
A pod rolled with a changed secret beside pods that hold only the old key refuses the same way, so the roll stalls on the new pod while the old ones keep serving.
Each step of a rotation (Identity, above) passes the check beside processes still on the step before.
A process whose heartbeat lapsed (frozen, or cut from the database, for longer than the live window) or whose row was pruned drops out of the registry while it runs, so a process started meanwhile is checked without it;
the first one checks its keys again before it counts as live once more, and beside a live process whose keys it does not share it logs `msg="dispatch: this server process no longer shares its signing keys with a live server process"` at ERROR, stops serving as a SIGTERM stops it (the listener closes at once, the requests in flight are answered, its row is deleted), and exits 1.

**Every migration leaves the release before it able to read and write the schema** (Database migrations, below):
a rolling update runs the old build and the new build against one database at once.
A change that cannot be written that way is a stop-the-world migration, deployed with no server of the earlier release running, and that is an exception named in the pull request.

**The release that moves live updates to `/ws/live` and keys every write (INFRA-258) deploys with no server of the earlier release running**, because of its API rather than its schema.
An earlier server ignores the `Idempotency-Key` header and keeps no key, so while both serve one database a write can run twice:
one an earlier server ran whose answer was lost is run again by its retry on a new server, which holds no key for it, and a retry of one a new server ran runs again on an earlier one.
The page and the `dispatch` command both retry a write whose answer was lost, and the command's check that Dispatch keeps keys (the key echoed back, or `/healthz`) reads whichever server answered it.
Stop the earlier release, or scale it to zero, before this one serves:
a Kubernetes Deployment's `strategy: Recreate`, which `envoy-infra:gcp:Dispatch` uses at its default one replica;
a deployment running more than one replica, which rolls, is scaled to zero first, as for `0117` (Database migrations, below).
The compose auto-deployer stops the old container before it starts the new one.
Releases after this one roll again.
A rollback to an earlier release brings the same hazard back, the other way:
a tab still open, and a `dispatch` client that saw this release keep keys within the last five minutes (`KEYED_LAYER_MEMORY_MS`), retry a write whose answer was lost against the earlier server, which runs it again.
Fix forward instead; if you must roll back, stop this release first, knowing a write in flight at that moment may land twice.
Once `0141_drop_broadcast_idempotency_keys` has applied, a rollback past this release also loses broadcasts:
the earlier build's broadcast send reads and writes the table `0141` drops, so every broadcast send fails while the rest of that build keeps working.
A tab opened before the deploy needs a reload:
`GET /api/v1/events` is gone, so it shows `Live updates unavailable`, and its broadcast body still carries `idempotency_key`, which this release refuses as an unknown field (`400 INVALID_JSON`).

What the two processes share, and what each keeps to itself:

- Each event is published once.
  The outbox claims a batch (`for update skip locked`) before it publishes it, so two processes never publish one event twice.
  A process that dies between a publish and its `published_at` write republishes once the claim's minute lapses, which is at-least-once on the `notifications.role.*` core-NATS lanes, whose receivers dedupe by key.

- A reader's events subscription on its live socket carries the other process's events.
  It is re-sourced from Postgres through a per-process relay on `listen dispatch_events` (migration `0107_events_notify`), so an event committed on either process reaches every open subscription with no reconnect.

- The two delivery durables keep their shape.
  NATS admits one binder; the second process stands by, asking every two seconds, and takes the durable over within seconds of the holder dying.

- No leased background loop runs a pass beside another process's pass of the same loop.
  Each pass takes a named session-level advisory lease for its length (`duty.Exclusive`), so a process whose attempt lands inside another's pass finds it held and logs a skip.
  The nine timer loops — the delivery reconcile, the architecture import, the webhook redelivery sweep, the outbox's daily document compaction, the block index reconciliation, the term-statistics refresh, the repair of closed and reopened issues' asks, the keyed writes' maintenance sweep (lease `suggestion-outcomes`) and the prune of the registry of live server processes
  — also record each pass in `duty_passes` (migration `0115`) with the loop's own period, so a process whose tick lands between another's passes
  — the ordinary case, since each ticker draws its phase at boot and a rolling update starts the second process minutes after the first
  — finds the period already served and logs the same skip: each runs once per period across the processes.
  The two leased woken loops, the reference excerpts and the edit notices, take their lease with no period:
  a process that finds it held tries again until a pass of its own runs, since the other process's pass may have read before the change that woke this one.
  The suggestion outcome sweep takes no lease at all:
  each of its pages holds the `suggestion_outcome_cursor` row `for update`, which serialises two processes by itself (Background loops, below).
  The settlement resumption, the delivery live hub, the event-stream relay, the stale-room evictor, the shared wake listener and the document rooms' timers stay per-process by design, and the embedding queue, the task progress reconciliation and the outbox's own scan claim their own work;
  the embedding queue's coverage pass runs in every process with no lease, each of its steps safe beside another process's, and drops every vector its own embedder did not make, so every process takes the same embedder settings (Search, below);
  `cmd/dispatch/main.go` says at the call site which loop is which and why.

- The registry of live server processes is shared.
  Each process stamps its `server_processes` row every 5 seconds, on a one-connection pool of its own, and counts as live while that heartbeat is within 15 seconds of the database's `now()`;
  it deletes its row once its HTTP drain has ended, so a draining process counts as live until its last request is answered.
  A row a process left behind (killed, its node lost) counts as live until 15 seconds after its last heartbeat, so a process refused beside it at boot waits up to that window and a heartbeat for it to lapse before it exits 1 (the signing key precondition, above);
  the prune loop deletes such a row 10 minutes after its last heartbeat, under the lease `server-processes`.
  A process whose heartbeat lapsed (frozen, cut from the database) or whose row was pruned checks its keys again, under the registry's lock, before it counts as live once more:
  it writes its row again, logging that it rejoined at WARN, when every live process shares its key set, and otherwise stops, after draining, and exits 1 (the signing key precondition, above).
  Operator subcommands never register.

- A sign-in started on one process completes on the other:
  its state travels in the browser's signed `dispatch_oauth_state` cookie, not in a process (Identity, above), given one `DISPATCH_SERVER_URL` on every process.

- `server.started` names the build every live process serves, once each time that build changes.
  A process that finds every live process serving one stamped, non-empty commit that the latest row of `server_build_announcements` does not name inserts a row;
  any process whose bus is connected publishes each row not yet published, with the event id `dispatch-server-started-<commit>-<seq>` and `started_at` the database's time of the announcement.
  Each process looks every 5 seconds, and an advisory lock keeps two from inserting or publishing at once.
  So a roll announces nothing until its last old process is gone, a restart of every process on one commit announces nothing, and a rollback to an earlier commit announces it again.
  A process whose NATS is down announces into the table and leaves the publish to one whose bus answers.
  Builds before this registry do not register:
  the first release that carries it announces as soon as its own processes agree, and a process of an earlier release that starts during that release still publishes its own start, so that release is deployed with no process of the earlier one serving beside it (production ran one pod with `Recreate` for it).

- Neither NATS nor an OIDC issuer gates the boot.
  A process whose NATS does not answer, or refuses its credential, starts and serves everything that does not need the bus, and each of its NATS clients connects in the background, retrying forever;
  the outbox publishes what was written meanwhile once the bus answers, the webhook redelivery sweep opens its KV bucket on its first pass with NATS connected, and the agent conversation relay answers `503 STREAM_UNAVAILABLE` until then.
  A sign-in issuer or service-account issuer that cannot be reached is discovered in the background (Identity, above);
  until then only what needs it answers `503`.
  `/healthz` reports each as `nats`, `signin` and `service_tokens`, and none of them fails the probe.

- Each build's dashboard assets are in the database.
  Every process publishes its web dist's assets at boot, before it listens (`internal/dispatch/dashboard`, migration `0136`), and answers a local miss under `/assets/` from there (Routes, below), so a page of one build served by one process is served its chunks by a process of another build.
  A process that cannot publish (the database refuses the write, or the build holds a file over 8 MiB, which CI's `bun run check:asset-sizes` in `packages/dispatch` refuses first) exits before it is ready, so a rolling update keeps the processes it would have replaced.
  A restart on a build already stored writes no bytes.
  The prune after each publish keeps every build a live server process serves and the 20 builds started most recently;
  a failed prune is logged and the boot goes on.

**Two processes do not share a live document session.**
Durable convergence is unconditional: every edit is appended under the document's advisory lock, Yjs updates commute, and the stored history is the union of them, so no edit is lost whichever process took it, no process writes a settlement or an ask retraction from a document state it does not hold (`ErrRoomBehindHead`), and a version whose room is behind the head is rendered from the stored document (`storedTree`).
What is per-process is the live room: while a room is resident in one process it does not receive the other's updates, so presence and cursors show only the peers on that process.
A write committed elsewhere closes the room within a notification round trip (`dispatch_doc_changed`, `docs/stale_room.go`), unless that room was evicted less than `staleRoomEvictInterval` (two seconds) ago, in which case its notification is refused and the room is re-examined when that window ends
— the case the trailing write of an editing burst meets, since the last notification of a burst is one of the refused ones.
Either way the browser reconnects, re-syncs and shows the current text.
A reader sees a brief reconnect, never stale text they can act on.
Cross-process live co-editing of one document — two rooms with text streaming between them — is a follow-up issue, out of scope here.

**A connection is closed after its next response once it is 60 seconds old** (`maxConnAge`, `cmd/dispatch/conn_age.go`):
the server stamps each connection's accept time and answers the first request it reads at that age or later with `Connection: close`, and a request carrying an `Upgrade` header, a websocket handshake, keeps its connection.
kube-proxy and the front door spread load per connection, not per request, so while callers reuse their keep-alive connections a process that has just rolled in gets little traffic;
a closed connection makes the client open a new one, which kube-proxy spreads across every ready process.
Shutdown needs none of this: once `Server.Shutdown` begins, Go's server already writes every response with `Connection: close` and closes the connection after it.
One race remains: a client can write a request onto an idle pooled connection at the instant the server closes it.
Clients retry an idempotent request on a new connection, but a proxied `POST` can come back as a `502`.

## Background loops

Dispatch's background loops come in three shapes, so that a database nobody writes costs them nothing except where GitHub gives them no change to wake on.
`cmd/dispatch/main.go` states the classes at the call site.

**Woken loops** run a pass at start and then only when a Postgres notification says their work may have changed, or when a due time their last pass asked for comes round.
With nothing written and nothing due they hold no timer and send no statement.
The four that run passes (`duty.RunWoken`) start two passes no closer together than the period of the ticker each replaced, so under load each runs no more often than that ticker did.

- The embedding queue (`embedqueue.Run`) scans when `embeddings_enqueue`, which every embedding trigger calls, writes a row:
  it notifies `dispatch_embeddings` (migration `0151`) for a new item or a text whose hash changed, and an update that leaves the text as it was notifies nothing.
  The rows the coverage pass writes wake it in process.
  Scans start at least 5 s apart.
  After each scan it reads when the earliest pending row falls due (a failed row's retry, or another process's claim lapsing after its two minutes) and arms one timer for it, never sooner than 5 s away.
  A throttled batch waits 5 s plus its backoff before the next.
  It takes no lease: its claim keeps two processes' scans off each other's rows.

- The reference excerpt reconciliation (`docs.Service.RunReferenceExcerptReconciliation`, lease `reference-excerpts`) is woken by a row trigger on `refs` (`0153`) that notifies `dispatch_excerpts_owed` for each document mention left owed an excerpt, whichever build or command wrote it (`rebuild-refs` included).
  Batches of 20 documents start at least 2 s apart, the next as soon as that allows while a full batch leaves documents it can still record.
  A document it cannot read or record is deferred in `reference_excerpt_backoff` (`0152`), keyed by the version that failed:
  one minute after the first failure, doubling to at most six hours, plus up to a fifth again of jitter;
  a success deletes the row, and a newer version starts again from one.
  A deferred document stays out of the batches until it falls due, so documents that keep failing neither hold the loop at 2 s nor keep the ones sorting after them waiting, and the earliest due time is the loop's only timer.
  Each process's first pass also marks owed the excerpts a pre-`0092` build left stale (`refs.MarkStaleExcerpts`), once.

- The edit notice sweep (`docs.Service.RunEditNotices`, lease `edit-notices`) is woken by a row trigger on `document_edit_notices` (`0157`) that notifies `dispatch_edit_notices` each time a writer gives a notice a due time:
  a person's settled version opening or moving its quiet window, from either build.
  A sweep sends the notices that are due, each as one `artifact.human_edited` event, and then reads the earliest due time left (`document_edit_notices_due`) and arms one timer for it, so a notice goes when its quiet window ends.
  Sweeps start at least 5 s apart, the period of the timer it replaced:
  a notice still due after a sweep (past a batch of 20, or one whose send failed) is swept again 5 s later, and a notice goes at most 5 s after its due time.
  A held window, which names nobody and has no due time, neither wakes it nor keeps a timer.

- The suggestion outcome sweep (`api.RunSuggestionOutcomeSweep`) is woken by `dispatch_events` (`0107`), which every event commit notifies whichever process made it.
  Passes start at least 60 s apart.
  A pass reads the events after its cursor (`suggestion_outcome_cursor`, `0154`) and at or below the event log's safe horizon, as the per-process relay behind a reader's events subscription reads:
  event ids are not commit order, so a cursor moved past an event that could still commit would never judge by it.
  The pass takes that horizon once (`events.Horizon.Safe`, the server's one `Horizon`;
  the first pass after a start takes `events.Horizon.Await`, which waits until the transactions holding an event id on the instance end, and ends with its context), and one left with an event above it asks for a pass 60 s on rather than re-reading at once, so a horizon an open transaction holds back costs one pass a minute;
  the late commit's own `dispatch_events` notification wakes the loop as any other does.
  A pass also reads the suggestions no pass has judged yet (`write_suggestions.swept`, `0155`), in pages of 1,000, each page in a transaction that holds the cursor row `for update`, and judges only the pending suggestions those can resolve:
  the newly recorded ones, against all their events, and those whose source issue, source document or suggested issue a page's event names.
  An event or suggestion less than 60 s old waits for a later pass, which the pass asks for as its due time, so an agent's writes within a minute are judged together.
  The first pass on a database, whose cursor is null, judges every pending suggestion once, as every pass of the release before it did.
  A failed pass is retried a minute later.
  Like the embedding queue, it takes no lease:
  the cursor row each page holds is what serialises two processes woken by one commit, the first pass included, since the row is there from `0154` with a null `event_id`.
  The second process's page waits on the row and then reads the cursor the first advanced, so it judges what the first did not rather than the same page twice, and no wake is given up to another process's pass.

- The delivery settings both GitHub intakes route by are one cache per process (`delivery.SettingsCache`), read at start and again when `delivery_settings` changes:
  `delivery_settings_changed` notifies `dispatch_delivery_settings` (`0156`) for a change to any column but the freshness ones every GitHub event and reconcile pass rewrite.
  A settings change reaches a running intake without a restart, and neither intake reads the row per message or on a timer;
  a read that fails is tried again 30 s later.
  The release before this one polls the row every 30 s and may run beside it during a roll.

The two leased woken loops take their lease with no period.
A process that finds it held tries again
— every 2 s for the excerpts, every 5 s for the edit notices, plus up to a tenth again
— until a pass of its own runs, because the other process's pass may have read before the change that woke this one.

**Boot passes** run once at start:

- The embedding coverage pass (`embedqueue.Cover`, Search below) runs once, with no lease.
  After it only its identity steps run again:
  at once when this process's queue first embeds successfully after the pass skipped them for a canary the embedder did not answer, and every `identityRecheckInterval` (10 minutes, plus up to a tenth again) while a run still finds rows of another identity, so a vector an old-configuration process commits during a roll is dropped.
  Once a run finds none, the loop holds no timer.

- The task progress reconciliation (`docs.Service.RunTaskProgressReconciliation`) counts every issue whose stored task count is not its latest version's.
  Every version write records its count in its own transaction, so nothing makes new drift while the process runs but a row another writer held during the pass:
  those are retried up to six more passes, the first a second later and doubling to at most a minute, and drift left after them is logged at WARN and left to the next start.

**Timers that stay**, because nothing announces what they would miss
— a webhook that never arrived, a delivery that failed, a source's branch head, an expired key, a delivery a process that died still owes:

- The delivery reconcile (`delivery.ReconcileInterval`) and the architecture import (`architecture.SyncInterval`) run every 5 minutes through `duty.RunJittered`, the first pass after a random delay within one interval.
  The webhook redelivery sweep (Webhook redelivery, above; `redeliver.Interval`) runs every 2 minutes, its first sweep at start.
  Each waits the interval plus up to a tenth again after every pass (`duty.Jittered`), drawn afresh every period, so two processes' passes drift apart rather than ticking in step.
  GitHub has no feed of what these three would miss.

- The keyed writes' maintenance sweep (`api.RunMaintenanceSweep`, lease `suggestion-outcomes`, `api.MaintenanceSweepInterval`) deletes the `Idempotency-Key` rows past their 24 hours and then sends every delivery a process that died left owed (Keyed writes, below), once a minute across the processes:
  its pass records its period in `duty_passes` like the timers above, so the other process's tick inside a period already served skips.
  No notification replaces this timer, which is why it is the one piece of maintenance that stayed one:
  a key passes its window by the clock alone, and a delivery is owed because the process holding it died, which commits nothing to announce.
  It keeps the lease name the suggestion outcome sweep carried this work under, so during a roll the earlier release's pass and this one still exclude each other on it.

Outside these classes, the outbox still scans on a 5 s retry ticker and compacts daily, the settlement resumption still runs every minute, the block index reconciliation (lease `block-index`) runs every 30 s (every 2 s while a batch is full) and the repair of closed and reopened issues' asks (lease `issue-ask-repair`, Database migrations below) every 10 minutes after its boot pass, each recording its passes in `duty_passes` with its period, and the per-feature listeners (the delivery live hub, the event stream relay, the stale-room evictor) keep their own connections and keep-alive pings.

**One shared listener per process.**
Every woken loop waits on one `pglisten.Hub` (owner `dispatch background wakes` in its log lines), which holds one connection from the store's listener pool and LISTENs on every loop's channel.
A channel is registered only for a loop that will run:
`dispatch_embeddings` only with an embedder, and `dispatch_delivery_settings` only with NATS and the GitHub App.
No loop waits for the listener: each runs its boot pass whether or not it has connected, and keeps its due-time timers while it has no connection.
Postgres does not replay a notification sent while nothing listened, so the hub signals every loop after each successful LISTEN, the first included, and a change made while it was redialing (backing off 1 to 30 s) is caught up then.

**The GitHub intake** (`delivery.Intake`) checks each message's subject before it decodes anything:
only `notifications.github.<owner>.<repo>.pr.<n>`, with nothing after the number, and `notifications.github.<owner>.<repo>.workflow.<file>.<action>` can carry a kind it handles, and every other subject
— checks, comments, issues, pushes, job events, `pr.<n>.review` — is acknowledged with no decode.
A wanted message runs under its key: a pull request under its repository and number, read from the subject, and a workflow run under its payload's `repo` and `run_id`.
Messages of one key run in delivery order, each after the one before has finished, since each fetches its pull request's or run's current state from GitHub and writes it, and run at once an older fetch could be written over a newer one;
messages of different keys run at once, at most 4, the durable's `MaxAckPending`.
Its durable's `AckWait`, `MaxAckPending` and filter subject are unchanged.
`Intake.StopDelivering` hands the durable over, and `Run` calls it when the process stops.
It drains the subscription the bus client holds for it
— after the client has replaced its connection, the one the client bound on the new connection
— and gives up the subscription's registration on the client, so the server delivers it nothing more, no reconnect binds it again and a later subscribe starts afresh (`bus.Client.DrainSubscription`), and holds every message waiting behind its key, and every message the client is handed meanwhile, until no handler is running.
The messages in their handlers get until the stop's deadline
— `intakeStopBudget`, 15 s after `Run`'s context ends, armed when it ends
— to finish and be acknowledged; the handlers still running then are given up, their GitHub calls cancelled, and their messages negatively acknowledged.
Only then does it negatively acknowledge the held messages, so NATS redelivers them at once to the next holder, which can then not start a pull request's or run's next message while this process still works on the one before it.
Last it waits for the drain to end, cut short by that deadline.
A bind under way when the context ends — a standby's poll
— spends the same deadline rather than starting one of its own:
its reconciliation's JetStream calls end with it, no subscribe starts once the context has ended, and the subscription of a subscribe already under way is stopped under that deadline.
The server's shutdown waits for that stop, which begins at the signal and whose 15 s from the signal is its only deadline, before it deregisters the process and closes the database pool:
a handler waiting on GitHub holds no connection, so nothing else would keep the pool open for its writes.

## Database migrations

Boot applies `internal/dispatch/store/migrations/*.up.sql` in version order and records each version in `schema_migrations`, which it creates under the runner's advisory lock, so processes booting together against an empty database migrate one after the other.
Version 8 is an empty file: that version was recorded from Go by the one-time conversion of pre-Proof `Y.Text` rooms and offset anchors into Proof trees and mark anchors, which every deployed database has already run.

**A migration must leave the release before it able to read and write the schema.**
A rolling update runs the old build and the new build against one database at once (Running more than one process, above), and the old build applies no compatibility shim:
nothing at boot refuses a schema newer than the binary knows, since the runner applies the versions it has not recorded and reads no other (`internal/dispatch/store/store.go`, `migrate`).
So a schema change is expand-then-contract across two releases.
The expanding migration adds, with a default or a backfill, and the release that ships it writes both shapes;
the contracting migration — one that drops, renames or narrows — ships no earlier than the next release, once no running build reads the old shape:

- A `not null` column is added with a default
  — a constant default rewrites no row — and the release that ships it writes the column on every insert;
  the release after it may drop the default, once no build inserts a row without the column.

- A rename is two migrations in two releases:
  the new column added and filled (a backfill for the rows already stored), its release writing both columns, then the old column dropped a release later.
  One `alter table … rename column` breaks every statement the earlier release still sends.

- A narrowing — a type made smaller, a nullable column made `not null`, a check added
  — ships behind a release that writes only the values the narrower shape admits, and narrows in the release after it.

A migration that cannot be written that way is a stop-the-world migration:
it is deployed with no server of the earlier release running, and that is an exception named in the pull request, not a default (`0113` and `0117` below are the two).
The pre-deploy census ("Pre-deploy census" below) is unchanged by that rule and still applies to every migration:
it is taken before any migration of the release applies, and a census that refuses fails the deploy before the service moves.
The rule in brief is `internal/dispatch/store/README.md`, beside the migrations directory rather than in it, since the loader below refuses a set holding a file named anything else.

A migration added from now on is named `<UTC timestamp>_<name>.up.sql`, the timestamp the moment its author created it, `YYYYMMDDHHMMSS` (`date -u +%Y%m%d%H%M%S`), with its census as `<same timestamp>_<name>.census.sql`.
The numbered versions are the sequence the set grew up with;
every one of those files keeps the number it landed with, and no number is added again:
the series closed at `0157_document_edit_notices_notify.up.sql`, which `migrationid` records once for Dispatch, and a set holding a number above it is refused (`migrationid.Check`), so a branch that takes the next number fails on its own, before it merges.
Two pull requests open at once used to take one number each and collide only when the first merged — nothing conflicts in git — while two timestamps taken a moment apart belong to their own authors.
The version is the file name's leading digits read as a number, as before, so every timestamp orders after every number and timestamps order among themselves by time.
A 14-digit version is past what an `integer` column holds, so `schema_migrations.version` is a `bigint`.
A database created before the change keeps its `integer` column until a boot whose set holds a timestamped migration, which widens it once, in the same locked transaction that creates the table (`createVersionTable`):
a boot with no timestamped migration leaves it alone, because the release before this change reads `max(version)` uncast and would fail that read once on every connection that prepared it while it still serves.
So this release is rolled out before any release that carries a timestamped migration:
a deployment that skips it costs the release it replaces one failed `/healthz` schema read on each connection that prepared the uncast read, which pgx then drops.
`schema_version` in `/healthz` is still `max(version)`, the highest version applied — which is not proof that every version below it is applied, since a migration can merge below one a database already has.
The auto-deployer's rollback guard compares the whole set of recorded versions for that reason, rather than `max(version)` (`packages/envoy/deploy/README.md`, "Auto-deployer").

A migration that merges below one a database has already applied still runs there, exactly once, at the next boot:
the runner applies every version `schema_migrations` does not record, not only those above the highest it does, each in its own transaction with its version row (`TestMigrateAppliesAMigrationThatArrivedBelowOneAlreadyApplied` and the proofs beside it in `internal/dispatch/store/migrate_out_of_order_test.go`).
Within one boot the runner applies the versions the database lacks in version order, so a fresh database applies the whole set in that order;
but a deployed database applies a migration when the release carrying it deploys.
If T2 deploys before T1 merges, that database applies T2 then T1 while a fresh one applies T1 then T2.
So a migration must not depend on its order relative to one written in a concurrent pull request, another branch open at the same time;
one that needs another's effect is written after that one has merged.

The runner records a migration by its version alone, so before it touches the database it reads the whole directory (`pgmigrate.Load`) and refuses to start, applying nothing and naming every file concerned, when two migrations share a version (it would apply the first and skip the rest as already applied;
keep the version on the file that merged to `main` first, since a database may already have applied it, and give each of the rest the UTC moment it was created), when a file is named other than `<version>_<name>.up.sql`, `<version>_<name>.down.sql` or `<version>_<name>.census.sql` (a `.down.sql` is a rollback script an operator runs by hand, and a `.census.sql` is the migration's census, "Pre-deploy census" below;
each needs its `.up.sql`), when a census is not one `select` or calls a function it may not (below), when a version is not decimal digits of a positive number the `bigint` column holds, and when a file cannot be read.
The directory is embedded with `all:`, which keeps a name beginning with `_` or `.`, so such a name is refused like any other.
An editor's swap file therefore stops a locally built `envoy-dispatch` from booting, and fails the store's tests, until it is removed, and a binary built while it was there keeps refusing to boot until it is rebuilt;
Vim writes its swap file beside the file it edits by default.
What no directive embeds, such as a symlink, a `.git` or a hidden name Go's module paths refuse, never reaches the runner, so only the store's tests catch it (`pgmigrate/pgmigratetest.CheckEmbedsEveryFile` names each cause).
Versions are applied by their number, not by file name, and the store's tests require every file on disk to be embedded (`TestEveryMigrationFileIsEmbedded`) and the identifiers to be the numbered migrations 1 to the number the series closed at (recorded once in `migrationid`), each taken once with none above it, and a real UTC instant above them (`TestMigrationSetTakesNumbersThenTimestamps`, `migrationid.Check`), both reading file names alone.

Every migration's lock waits are bounded at five seconds (`pgmigrate.LockTimeout`, which `pgmigrate.Exec` sets with `set_config(…, true)`, local to the one transaction the runner applies that migration in, after the runner's advisory lock), so a migration queued behind a long transaction gives up instead of holding every read and write of its table behind its request.
The runner then rolls that transaction back, holds nothing for two seconds (`pgmigrate.LockTimeoutRetryPause`) so the requests queued behind it run, and tries again, up to three attempts (`pgmigrate.LockTimeoutAttempts`, `pgmigrate.RetryLockTimeouts`).
Each timed-out attempt is logged at WARN.
A migration that times out on every attempt fails the boot; it has then held its table's reads and writes for at most three waits of five seconds.
The failure names the migration, the lock it wanted, the sessions it was queued behind with each one's diagnosis (as the census's refusals carry it, "Pre-deploy census" below), and a `pg_stat_activity` query for the sessions holding the lock;
end the holder or let it finish and start the server again.
That query can come back empty: a prepared transaction's locks carry no pid, so the join to `pg_stat_activity` drops them (LEGION-432).
A migration that needs another bound sets its own `SET LOCAL lock_timeout`, which each attempt sets again.
The secrets broker's runner applies every migration in one transaction, so it does not try again:
rolling one attempt back would roll back the migrations before it too.

The notes below describe migrations through `0148`.
A new migration describes itself in the header comment of its own `.up.sql`, as `0129_document_edit_notices.up.sql` does, and adds nothing here, so two pull requests that each add a migration do not edit this section.

Migrations `0056`–`0062` make search indexing linear.
Each table has its own migration, so its transaction holds an `ACCESS EXCLUSIVE` lock only for the `DROP EXPRESSION` and trigger setup;
`0062` re-indexes only rows holding sixteen or more underscore-joined segments under `ROW EXCLUSIVE`.
Their censuses answer `0` for 0056–0061, which neither refuse nor rewrite a row, and, for 0062, the candidate rows whose stored vector the new expression changes.

Migration `0063_doc_settlements_pending` creates the table in which every durable document update records the settlement it owes, which the settlement deletes when it commits.
A room's load and the server's minute-by-minute resumption arm the settlement a row names, so one a shutdown cuts short still runs.
It creates a table and touches no row; its census answers `0`.

Migrations `0064`–`0066` let an approval request follow its document's versions.
`0064` backfills `requested_version` into every approval ask from its `version`, rebuilds `asks_approval_kind_check` to require it, and adds the nullable `asks.handed_back_reply_id` and an insert trigger that gives an older binary's approval row the same value; it locks `asks` alone.
`0065` sets `comments.created_at`'s default to `clock_timestamp()`, so a comment is stamped when its insert runs, after the owner row every comment insert takes, and an ask's newest reply is the one that committed last; it locks `comments` alone.
`0066` adds `handed_back_reply_id`'s foreign key to `comments`, which takes `SHARE ROW EXCLUSIVE` on both tables, as adding any foreign key does:
a write waits behind it, a read does not.
In one migration, the column's key held `asks` `ACCESS EXCLUSIVE` while it waited for `comments`, and a comment write that went on to read `asks` deadlocked with it.
A comment write that goes on to write `asks` (accepting a suggestion, or an edit under a comment's anchor, whose version moves the approval request) can still close a cycle with `0066` while it waits for `comments`, and under the five-second bound either side could lose it:
Postgres checks a waiter once, `deadlock_timeout` (1 s by default) after it starts to wait, so a write that closes the cycle within that second fails `0066` and one that closes it later is itself the victim, answered `500`.
`0066` therefore sets its own `lock_timeout` of 500 ms, shorter than `deadlock_timeout`:
it gives up first (`55P03`, nothing applied), the write finishes, and the runner's next attempt, or the next boot, applies it;
the cost is that a comment write holding `comments` past half a second on every attempt fails that boot too, and so does an autovacuum of either table where `deadlock_timeout` is not shorter, which the census refuses (below).
Every census answers `0`: 0053's check guarantees every approval ask a `version` to backfill from, and the other two change no row.

Migration `0067_user_agent_reply_read` creates the table of replies a human has read by id, which the broadcast page writes for the replies it shows and the unread count leaves out, beside the per-session read mark, and its `(login, session_id)` index, which the read mark's prune of one session's rows reads.
It creates a table and touches no row; its census answers `0`.

Migration `0068_people` creates `people`, everyone who has signed in, holding the sign-in pool's refresh token for a person who signed in through it, and drops `users`, the GitHub OAuth token pair of each GitHub login that signed in, which the GitHub proxy acted with before it read GitHub as the App.
It locks `users` alone, `ACCESS EXCLUSIVE` for the drop; `people` is new.
Its census counts the `users` rows the drop deletes.

No migration retires the refresh tokens that `0068`'s release stored in plain text:
every boot does (Identity, above), since a task of that release can write one during the roll, or after a rollback, once every migration has run, and a token written after a boot waits for the next one.
So once the last task of the earlier release is gone, the deployer restarts Dispatch once and then checks three things:
that boot logged `failed=0`; no `a sign-in replaced a refresh token the pool did not revoke` line has appeared since the roll began (each one names a person to sign out at the pool);
and `select count(*) from people where refresh_token is not null and refresh_token not like 'v1:%'` answers `0`.
A boot line with `failed=<n>` above zero names each token it could not revoke in an ERROR line;
the next boot tries again, and signing that person out at the pool ends it at once.
A non-zero count with no such line is a token a task of the earlier release wrote after the last boot, which another restart retires.

The boot revokes only the plain-text tokens still in `people`.
Under `0068`'s release a sign-in that replaced a person's token, or a logout that cleared it, removed it without revoking it, and it stays valid at the pool until it expires in any backup that holds it.
So if any database backup was taken between `0068`'s deploy and the post-roll restart, either delete those backups or sign out at the pool everyone whose `people.signed_in_at` is after `0068`'s deploy.

Migration `0071_agent_artifacts` gives an artifact a third owner, an agent's conversation:
the nullable `artifacts.session_id`, a check that every artifact has exactly one owner (an issue, `issue_key` with its `project_key`;
a project, `project_key` alone; or a session, a non-empty `session_id` alone), and `project_key` made nullable for the session's rows.
`ref_key`, 0009's generated column, becomes a plain column that a `BEFORE INSERT OR UPDATE` trigger (`artifacts_ref_key`) fills, as `0058` did for `search`:
`DROP EXPRESSION` keeps every value and rewrites no row, the trigger computes 0009's expression for every other row and `agent/<session id>/<slug>` for a session's, and `artifacts_ref_key_key` keeps it unique.
It adds the partial index `artifacts_session_id`, which serves the two lookups an upload to an agent's conversation makes by `session_id` (its artifact by name, and whether a slug is taken).
It locks `artifacts` alone, `ACCESS EXCLUSIVE` for milliseconds while the check reads its metadata rows;
its census counts the rows the check would refuse, `project_key is null`, which 0009's `not null` keeps at `0`.

Migration `0082_answers_by_person` adds two partial indexes for `GET /api/v1/me/answers`:
one on `events` for user-authored `ask.answered` events, which the route filters to events whose actor is the answer's user, and one on `comments` for user replies on asks.
Building them takes `SHARE` on `events` and `comments`; its census answers `0` because it refuses and rewrites no row.

Migration `0087_search_vector_bound` creates `search_vector` and replaces the bodies of four search triggers' functions (documents, comments, asks and messages) so that each builds its vector with it ([Search](#search)).
Replacing a function locks no table, and for every text whose vector fits, which is every row already stored, the new functions build the vector the old ones did, so it re-indexes nothing; its census answers `0`.

Migrations `0088_issues_title_lexemes` and `0089_issues_title_lexemes_backfill` store each issue title's lexemes, which the duplicate-title check reads ([Search](#search)).
`0088` adds `issues.title_lexemes` with the constant default `{}`, a catalog change that rewrites no row, so it holds `issues` `ACCESS EXCLUSIVE` for milliseconds (3 ms on 2,905 issues with production's titles), creates `title_lexemes(title)`, the one definition of a title's lexemes, and has the issues trigger build `search` with `search_vector` and fill the column on every insert and every retitle.
`0089` fills it on every issue stored before, under `issues` `EXCLUSIVE` taken before it writes a row:
a read does not wait, while every write of an issue's row, and of a row that references an issue, waits until it commits (0.58 s on those issues at load 85).
The update alone locked each issue's row in the order rows lie on disk, which a reparent, locking its two issues in key order, could cross:
on 60,000 issues the two deadlocked (`40P01`) in each of two runs, and with the table lock the reparent waited for the migration and committed.
Neither refuses a row; both censuses answer `0`.

Migration `0091_artifacts_project_documents` adds the partial index `artifacts_project_documents`, on `artifacts (project_key)` where `issue_key` and `session_id` are null: a project's unlinked documents.
Settlement of a project document holding an ask block no ask of its own indexes reads its project's other documents through it, for the ask the block may be a copy of and for the copies of an ask it retracts, and so do the answer and resolve routes, for the copies of the ask they close (LEGION-651).
Building it takes `SHARE` on `artifacts`; its census answers `0` because it refuses and rewrites no row.

A decided ask that a release before LEGION-651 reworded gets back, in code, the wording its decision was given on:
the `previous` wording of the first `ask.edited` after its latest `ask.answered` or `ask.resolved`, with no `ask.opened` between.
No migration rewrites those rows, since the census refuses one that does (Pre-deploy census, below).
Every boot runs the restoration over every decided ask in one statement (`docs.Service.RestoreDecidedWordings`, bounded at 30 s), reading `events` through `events_ask_payload_id` and writing only the rows whose wording differs;
it logs `dispatch: restored a decided ask's wording` for each with the ask, its document and the question it asks again.
In the same transaction it marks owed, in one upsert, the settlement of each restored block ask's own document, whose block still asks the rewording beside the decision, and of every other document of each restored ask's owner whose latest version holds the ask's block with no ask of its own under that id, a copy that may have matched the rewording and shown the decision on it, and arms those settlements once it commits; then it logs `dispatch: restored
decided asks' wordings` with `restored=<n> copies=<n>`.
On the 2026-10-11 production dump (3,739 asks, 82,153 events) it restores 5 asks, each a block ask on a document of its own, and marks 0 copies owed, in 0.17-0.77 s:
the one other document holding one of those 5 blocks has an ask of its own under that id, so it is no copy, and the first boot of the release that carries it logs `restored=5 copies=0` and settles those 5 documents;
each whose block still asks the rewording gets the decided wording written back and logs `dispatch: wrote a decided ask's wording back into its
block`.
Each document's settlement runs the same statement over its own decided asks before it reconciles them, and marks the restored asks' copies owed the same way, so a rewording a task of the earlier release writes during the roll is undone by that document's next settlement on this release, which also writes the decided wording back into the block and settles its copies again.

A close by a release before the withdrawal left its issue's asks open, and a reopen by one left the asks a close withdrew resolved.
No migration writes the withdrawals, since the census refuses one that rewrites rows (Pre-deploy census, below);
code repairs them instead (`api.RepairIssueAsks`).
Every boot runs a pass, bounded at 30 s, and then every 10 minutes (`api.IssueAskRepairInterval`) one process runs another under the lease `issue-ask-repair`, bounded at 5 minutes, for the life of the process.
Each issue is repaired in its own transaction holding the issue's row:
a closed issue holding open asks gets them withdrawn exactly as a close withdraws them, attributed to the actor of its latest `issue.closed` (an issue with no such event is logged and skipped), so the asks left open on issues closed before the release are withdrawn naming each issue's earlier closer;
an open issue holding `withdrawn` asks gets them restored, attributed to the actor of the event that reopened it;
and a closed issue that holds open asks, or whose documents owe a settlement marked since the last pass (at boot, the last 10 minutes;
on the first interval pass, any), gets the close's final sweep in the same write of each document, which writes block `state` attributes only, never an ask row or a record.
A document whose stored history does not load is logged and left as it is, as the close leaves it.
Each record is dated when the pass writes it.
A pass logs `dispatch: repaired the asks of closed and reopened issues` with `withdrawn=<n> restored=<n>
swept=<n>`; one that fails or runs out of time is logged, and the next pass repairs what it left:
each interval pass that listed its candidates moves the bound to one interval before its own start, and carries every issue it did not repair, failed or not reached, into the next pass.
The bound and the carried issues live in the memory of the process that ran the pass, not in the database:
when the lease moves to another process, that process's pass starts from its own bound and its own carried issues (on its first interval pass, no bound, so it sweeps every closed issue whose documents owe a settlement), and never sees the issues the other process carried.

Migration `0092_refs_excerpts` adds three nullable columns to `refs` with no default (`excerpt_block_id`, `excerpt_text`, `excerpt_version`), a catalog change that rewrites no row, and the partial index `refs_excerpt_drift` over the rows whose `excerpt_version` is null:
the stored excerpt `GET /api/v1/references` answers a document mention with, which every version write fills and the reference-excerpt reconciliation fills for every row stored before it (`packages/envoy/AGENTS.md`).
On the 2026-10-10 production dump the reconciliation's first pass filled 7,498 mentions of 2,150 documents in 248 s at `GOMAXPROCS=2`.
Its census answers `0`.

Migration `0094_issue_links` creates `issue_links`, the issues each issue waits on through `blocked_by`, and its target index, and re-states the `graph_edges` view with the `blocked_by` arm.
The table's two foreign keys take `SHARE ROW EXCLUSIVE` on `issues` for the migration's short transaction, so an issue write waits behind it while an issue read does not, and re-stating the view holds `graph_edges` `ACCESS EXCLUSIVE`, so a reference read waits too.
It touches no row; its census answers `0`.

Migrations `0095_user_threads` through `0099_user_threads_catch_up` build `user_threads`, the table behind the Replies page (`packages/envoy/AGENTS.md`).
`0095` creates the table, its partial index `user_threads_unread` and its `(thread_id)` index, the functions `user_threads_login`, `user_threads_record`, `user_threads_comment` and `user_threads_message`, and the trigger on `comments`;
`0096` adds the trigger on `asks` (`insert or update of answer`); `0097` attaches `user_threads_message` to `messages`.
Each trigger is its own migration, so its transaction holds that one table's `SHARE ROW EXCLUSIVE` alone:
a write to it waits behind the migration and a read does not.
`0098` seeds a row for every person and thread they wrote in, under `ACCESS SHARE` on `comments`, `asks`, `messages`, `issues` and `artifacts`, writing only `user_threads`;
every row starts read, its read mark moved to the newest reply already stored, so the deploy turns no stored reply into an unread one.
The triggers are live by then, so a write that lands while it runs on a thread whose participant already has a row is caught by the trigger, and the seed merges with it by taking the later of each time.
A participant the seed is still inserting has no row the trigger's update can see, and the seed's snapshot cannot see that write, so `0099` reads the same writes under `ACCESS SHARE` on `comments`, `asks` and `messages` and moves each row to any write by someone else newer than its `replied_at`, which makes it unread past its read mark;
it moves nothing backwards, and a write the seed saw stays read.
On a copy of the 2026-10-12 production dump (11,497 comments, 3,882 asks, 14,289 messages) the seed wrote 3,819 rows for three logins, all of them read, in 0.93 s, and `0099` ran in 0.11 s under the server's own runner and moved no row, taking `ACCESS SHARE` on the three tables it reads and `ROW EXCLUSIVE` on `user_threads` alone.
All five censuses answer `0`: `0095`-`0097` create functions and triggers and refuse or rewrite no row, and `0098` and `0099` write only the table `0095` creates.

Migration `0113_repo_projects_default_and_issue_repo` adds `repo_projects.is_default` and `issues.repo` (`packages/envoy/AGENTS.md`).
From its release on, an issue sits at most 32 links below its root, and the API refuses a write past that (`409 PARENT_INPUT`);
the release before it did not, so `0113` refuses to apply while any issue sits more than 32 links below its root or in a parent loop, naming up to 20 of them and the repair:
`PATCH /api/v1/issues/<ancestor>` with `{"parent": null}`, on the release still running.
Its census counts the same issues.
It takes `repo_projects` and `issues` in `ACCESS EXCLUSIVE` before it counts and holds both to commit, so a write of the earlier release that commits while it waits is in the count.

**`0113` was deployed with no server of the earlier release running, and no release older than it is started over it again.**
That instruction was `0113`'s own, not the rule for `0114` and after:
`0113` is applied in production, and a release whose migrations are written to the rule above rolls (Running more than one process, above).
Its own deploy stopped the earlier release, or scaled it to zero, before the new release booted and migrated
— for that deploy, a Kubernetes Deployment's `strategy: Recreate`.
A failed deploy recovers forward, not by rolling the image back.
Only the new release's API holds the limit; nothing in the database does.
Boot has no check that refuses a schema newer than the binary knows:
the runner applies the versions it has not recorded and reads no other (`internal/dispatch/store/store.go`, `migrate`).
So an earlier release left serving after `0113`, or started again over it by a rollback, writes as it always did, and can build a chain past the limit again.
Such a chain, or one written by direct SQL, fails every read of its issues with `500`, a project's list included.
The API repairs an acyclic one: `PATCH` an ancestor that still reads (its `GET` answers `200`) with `{"parent": null}`.
The `PATCH` reads the issue it changes first (`api/issue_patch.go`), so a parent loop none of whose members reads, which only direct SQL can write, needs a database repair:
`update issues set parent_key = null where key = '<a member>'`.
The auto-deployer (`packages/envoy/deploy/README.md`) keeps both rules.
Compose stops and removes the old `dispatch` container before it starts the new one.
When a deploy fails, the auto-deployer stops the failed image and confirms the stop before it reads the schema, and when the schema has moved past the one the previous image runs against, it refuses to start that image again and exits `1` for a forward recovery.

Migration `0117_architecture_sources_per_repo` lets a project import its components from more than one repository (`packages/envoy/AGENTS.md`).
`architecture_sources` gains `id`, which names a source within its project, and its primary key moves from `(project_key)` to `(project_key, id)`, with a unique index on `(project_key, repo)`, so a repository is a source of a project at most once.
`components` gains `source_id` and a foreign key on `(project_key, source_id)` to its source's row, `on delete cascade`, so deleting a source deletes its components.
Both columns are added with the constant default `0`, which rewrites no row, and the default is then dropped:
every existing source reads `id` 0, every existing component `source_id` 0 (its project's one existing source), and a source added later gets an `id` from 1 on.
It takes `architecture_sources` and `components` in `ACCESS EXCLUSIVE` first and holds both to commit.
Its census counts the components whose project has no source, the rows the foreign key would refuse:

```sql
select count(*) from components c
where not exists (select 1 from architecture_sources s where s.project_key = c.project_key);
```

Only direct SQL writes such a component: the importer writes components under their source's row lock, and deleting a source deletes its components.
A census that refuses on it is repaired on the release still serving, before the deploy, by deleting them:
`delete from components c where not exists (select 1 from architecture_sources s where s.project_key = c.project_key)`; then take the census again.

**`0117` is a stop-the-world migration: deploy it with no server of the earlier release running, and never start a release older than `0117` over it again.**
Stop the earlier release, or scale it to zero, before the new release boots and migrates:
a Kubernetes Deployment's `strategy: Recreate`, which `envoy-infra:gcp:Dispatch` uses at its default one replica (`packages/envoy/infra/README.md`);
a deployment running more than one replica, which rolls, is scaled to zero first.
The compose auto-deployer above stops the old container before it starts the new one.
A failed deploy recovers forward, not by rolling the image back.
The earlier release does not work over the new schema, and nothing at boot stops it from serving:

- Its save of a project's source upserts `on conflict (project_key)`, which no unique index covers once `0117` commits, so Postgres refuses the statement and the save answers `500`.

- Its import takes one source row of the project as the project's one source, whichever row Postgres returns first.
  When the model changed, it deletes every component of the project, every source's, and inserts that source's components with no `source_id`, which has no default.
  A model holding a component fails on the not-null column and rolls back, and its failure record writes the error onto every source row of the project.
  A model holding none (no file in that branch's `.dispatch/architecture/`) commits the delete, and every source's components are gone.

- Its delete removes every source of the project and every component of the project.

Migrations `0118_embeddings_pending_since` and `0119_drop_embeddings_backfill_progress` move meaning search's coverage into the server (Search, below).
`0118` adds `embeddings.pending_since` (`not null default now()`:
when the row last became pending, which the coverage route's oldest wait reads;
a row pending at the migration takes its own `created_at`) and `embeddings.dead_model` (nullable:
the embedder identity the row was confirmed dead under), and replaces `embeddings_enqueue` so that a text change also starts the row's wait again and clears `dead_model`.
A row already dead at the migration records `us.cohere.embed-v4:0`, the Bedrock identity every earlier release embedded with, so a deployment that keeps Bedrock does not re-admit it, while a switch to Azure re-admits it once;
a row the earlier release confirms dead during the roll records none and gets the new release's attempts once.
Both columns are additive and the earlier release names neither: it inserts only through `embeddings_enqueue` or with an insert the default fills.
`0119` drops `embeddings_backfill_progress`, the checkpoints of the removed `backfill-embeddings` subcommand.
Dropping a table in the same release as the code that stopped using it is what the rule below forbids for a table a running build reads;
no server of any release read or wrote this one, only that person-run subcommand did, so the drop breaks no build still serving through the roll.
Both censuses answer `0`: `0118`'s updates write only the columns it adds, and `0119` drops nothing a running build still reads.

Migration `0120_artifact_reviews_event_seq` adds `artifact_reviews.event_seq`, the sequence of the `artifact.*` event each review appended on its document's owner, which `docs.WriteReview` writes in the transaction that appends the event and the approval read returns as `seq` (`packages/envoy/AGENTS.md`).
It is nullable, so a review recorded before it reads with no `seq`, and the release before it, which names no `event_seq` in its insert, rolls beside it.
Adding a nullable column with no default writes the catalog alone, under `artifact_reviews`' `ACCESS EXCLUSIVE` lock for milliseconds;
it refuses and rewrites no row, and its census answers `0`.
Its `event_seq > 0` check is a second statement, added `not valid` as the rule below requires:
an inline check would read every row of the table while it held that lock, and `not valid` still refuses every row written or updated after it.
Nothing validates it later — every row below the migration carries no sequence at all — so the constraint stays `not valid` for good.

`0120` also creates `artifact_reviews_by_artifact_event_seq` on `(artifact_id, event_seq desc nulls
last, created_at desc, id desc)`, `docs.ReviewOrder` per document, so the newest-review reads (`docs.LatestApprovedReview`, `latestReviewBy`) take one index entry rather than sort every review of the document.
It is a plain `create index`, not `concurrently`:
the runner applies a migration inside one transaction (`internal/dispatch/store/store.go`, `pgmigrate.Exec`), where `concurrently` cannot run, and no migration of the set uses it.
The build takes no lock the migration does not already hold
— its `alter table` statements hold `artifact_reviews`' `ACCESS EXCLUSIVE` — and holds it while it reads the table's rows, which the census bounds by its 1 GiB limit;
the census still answers `0`, since the build refuses and rewrites no row.
`artifact_reviews_by_artifact` (`0014`, `created_at desc, id desc`) stays, because the release before `0120` orders by it while it serves.

The release before `0120` keeps serving beside it while the app Deployment rolls (below), and again for as long as a rollback lasts, and it inserts every review naming no `event_seq`.
Left null, such a review — newer than every sequenced one
— would rank below them all in `docs.ReviewOrder` (`event_seq desc nulls last`):
a person's approval an old pod wrote would read back as the changes requested before it, and a changes request an old pod wrote would be overridden by the approval before it.
So `0120` also adds a trigger, which the rule below allows a migration to add:
`artifact_reviews_event_seq`, a constraint trigger on `artifact_reviews` deferred to the commit, gives every review inserted without a sequence, by either release, the sequence of the `artifact.*` event its own transaction appended for it (its type, document and version, among the events on the owner that its transaction wrote, which the owner's row lock keeps the newest there until the commit).
An event is the transaction's own when it carries the transaction's `now()` and its row's `xmin` is the transaction or one of its subtransactions (`artifact_reviews_own_row`, which reads it through `pg_xact_status`, so a savepoint's event counts and another transaction's does not, even one that started in the same microsecond and so shares `now()`).
It is deferred because every release inserts a review before it appends the event.
Every row that carries no sequence was therefore committed before `0120`, whose `ACCESS EXCLUSIVE` lock waits out every transaction that has inserted a review, and every sequenced row is newer.
The trigger is on `artifact_reviews`, the table `0120` already locks, so the migration holds no second lock:
a trigger on `events` would need that table's `SHARE ROW EXCLUSIVE` while holding `artifact_reviews`, against a transaction that appended an event and then waits to insert a review (a carried approval), and the census holds every table a migration names to its 1 GiB limit.

`0120` is additive — a nullable column, a `not valid` check, an index and a trigger
— so rolling Dispatch back past it is an image rollback alone, with no schema to restore and no backup to take for it:
the release before it runs over the new schema unchanged, and the trigger gives every review that release writes its sequence.
The auto-deployer is the exception, by its own rule above: once the schema has moved it refuses to start the previous tag, and recovery there is forward.

Migration `0121_block_graph` creates the tables behind a document's blocks as graph nodes (`packages/envoy/AGENTS.md`, "References form one graph"):
`document_blocks`, `block_refs`, `document_block_versions`, `document_block_texts` and `document_indexed_versions`, each with a foreign key to `artifacts`, and `block_grammar_owed`, with their indexes, among them the partial `document_block_versions_owed` and `block_grammar_owed_enqueued` through which the block-index reconciliation reads what is owed.
It creates the trigger function `block_grammar_owe`, which no trigger calls until `0123`, and re-states the `graph_edges` view with two more arms:
a block's mentions and its `part_of` edge to its document.
The five foreign keys take `SHARE ROW EXCLUSIVE` on `artifacts` for the migration's short transaction, so an artifact write waits behind it while a read does not, and re-stating the view holds `graph_edges` `ACCESS EXCLUSIVE`, so a reference read waits too.
Its one insert marks every document owed a block index in `document_block_versions`, a table it creates, and the server's reconciliation indexes them with no command run.
It refuses and rewrites no existing row; its census answers `0`.

Migration `0122_artifact_versions_block_index_trigger` creates `document_block_versions_owe` and an `after insert` trigger on `artifact_versions` that marks a document owed a block index whenever any build inserts a version carrying markdown.
A version a build without the block index writes during the roll is then indexed by the reconciliation, and a block-aware build's own index of the version, later in the same transaction, settles the mark.
The trigger takes `SHARE ROW EXCLUSIVE` on `artifact_versions` alone, so a version write waits behind it while a read does not.
It touches no row; its census answers `0`.

Migration `0123_comments_block_grammar_trigger` attaches `block_grammar_owe` to `comments`, `after insert or update of body`, for a body that holds `#b-`:
a comment any build writes that may cite a block is owed an index with the block grammar, which a build that predates the grammar does not give it.
The trigger takes `SHARE ROW EXCLUSIVE` on `comments` alone, so a comment write waits behind it while a read does not.
It touches no row; its census answers `0`.

Migration `0124_asks_block_grammar_trigger` does the same for `asks`, `after insert or update of
question`, for a question that holds `#b-`.
Its trigger takes `SHARE ROW EXCLUSIVE` on `asks` alone, so an ask write waits behind it while a read does not.
It touches no row; its census answers `0`.

Migration `0125_messages_block_grammar_trigger` does the same for `messages`, `after insert or
update of body`, for an issue message (`issue_key` set) whose body holds `#b-`; a direct message is indexed by nothing, so it owes nothing.
Its trigger takes `SHARE ROW EXCLUSIVE` on `messages` alone, so a message write waits behind it while a read does not.
It touches no row; its census answers `0`.

Migration `0126_block_grammar_seed` owes the same index to every comment, ask and issue message already stored whose text holds `#b-`, read with the three triggers' predicates.
The triggers are live by then, so a write that lands while it runs is owed by its trigger, and a row a trigger has already written keeps its `enqueued_at`.
It takes `ACCESS SHARE` on `comments`, `asks` and `messages`, which no write waits behind, and writes only `block_grammar_owed`.
Its census answers `0`: the one table it writes is `0121`'s, which holds no row at deploy, and the rows it inserts are new work owed, not a rewrite.

Migration `0127_asks_block_id_key` creates `block_id_key(id)`, the SHA-256 of a block id's UTF-8 bytes, written to be immutable so an index can use it, and rebuilds `asks_block_id_unique` (`0018`) and `asks_anchor_block_id_idx` (`0023`) under their names over it.
A B-tree entry holds at most 2704 bytes, and both indexes keyed the id itself, so an ask block or an ask anchored in a block whose incompressible id a browser's edit made longer failed its write, a settlement opening that ask included;
under the 32-byte key every such write is written.
`asks_block_id_unique` still keeps `(block_artifact_id, block_id)` unique under its name, and each index keeps the first column every read of it leads with, so the earlier release writes and reads through them as before.
Each drop holds `asks` `ACCESS EXCLUSIVE` until the migration commits, so every read and write of an ask waits while the two indexes build over its rows.
It refuses and rewrites no row: rows the old unique index held apart differ in their id, so they differ in its SHA-256.
Its census answers `0`.

Migration `0128_comments_anchor_block_id_key` rebuilds `comments_anchor_block_id_idx` (`0023`) over `0127`'s `block_id_key` the same way, so a comment anchored in a block whose id is any length is written.
Its own migration, so its transaction holds `comments` alone:
the drop holds it `ACCESS EXCLUSIVE`, and every read and write of a comment waits while the one index builds.
It refuses and rewrites no row; its census answers `0`.

Migration `0129_document_edit_notices` creates `document_edit_notices`, one row per document holding a person's pending edit notice:
the versions its quiet window spans, every person who wrote one, the actor the notice's event carries and when the window ends.
A settlement of a person's browser edit opens or moves the row in its own transaction, and the leased edit-notice sweep deletes each due row and appends its `artifact.human_edited` event in the same transaction, so a notice still pending when a process stops is sent by either process's next sweep.
It also adds `doc_settlements_pending.last_person`, the latest person sender of the document's pending edits, which names who edited last.
The table is new and the column a plain nullable add, so no row is refused or rewritten and its census answers `0`;
the foreign key takes `artifacts`' lock for the length of the migration, and the alter waits for `doc_settlements_pending` within the five-second lock bound.

Migration `0130_comments_written_version` adds `comments.written_version`, the version of its document current when a comment was written on it without an anchor (the comment's graph node's `parent_version`, [Reference graph](#reference-graph)), and a `before insert` trigger that sets it to the highest `artifact_versions.number` of the document on every comment with `artifact_id` set and `anchor` null whose insert names none, so the earlier release's inserts during the roll record it too.
The column is nullable with no default, a catalog change that rewrites and scans no row;
its own migration, so its transaction holds `comments` `ACCESS EXCLUSIVE` alone, for milliseconds.
It refuses and rewrites no row; its census answers `0`.

Migration `0131_asks_written_version` does the same for `asks`:
its trigger sets an approval ask's `written_version` to `approval.version`, the version the request names when it opens, which stays while the row's `approval.version` moves with later versions, and that of an ask with `artifact_id` set, `anchor` null and `block_artifact_id` null to the highest version of its document.
Its own migration, so its transaction holds `asks` `ACCESS EXCLUSIVE` alone, for milliseconds.
It refuses and rewrites no row; its census answers `0`.

Migration `0132_component_facts` creates `component_facts`, when each component was made and last changed and the digest of its own fields at its last recorded change, keyed `(project_key, id)` as `components` is, which every import keeps from then on (`packages/envoy/AGENTS.md`, "References form one graph").
Its foreign key to `projects` cascades, and it has none to `components`, whose rows every import deletes and inserts again.
Its one insert seeds a row with all three null for each component already stored: when those were made or last changed was never recorded.
It reads `components` under `ACCESS SHARE`, and the foreign key takes `SHARE ROW EXCLUSIVE` on `projects` for the catalog change.
It refuses and rewrites no existing row; its census answers `0`.

Migration `0133_comments_written_version_backfill` fills `written_version` on the comments with `artifact_id` set and `anchor` null stored before `0130`'s trigger:
each takes the highest version of its document created at or before it, the only record those rows have, so a comment written while a version's transaction was still open can name that version.
A comment the trigger recorded keeps its value, one on a document with no version that old keeps null, and no update trigger fires.
It takes `comments` `EXCLUSIVE` before it writes a row, as `0089` takes `issues`, so no write that locks two of its rows in another order deadlocks with it:
every read goes on, and every comment write waits until it commits.
Its census answers `0`: it fills a column only this release reads and changes no value any reader had, and it cannot name the column, which does not exist when the census is taken.

Migration `0134_asks_written_version_backfill` does the same for `asks`, holding `asks` `EXCLUSIVE`:
an approval ask takes `approval.version` of its `ask.opened` event (the type list of `events_ask_payload_id`, `0030`, lets the lookup read that index), the version the request named when it opened, and keeps null without one;
another ask with `artifact_id` set, `anchor` null and `block_artifact_id` null takes the highest version of its document created at or before it, as `0133` reads a comment's.
Its census answers `0` for the same reasons.

Migration `0135_server_processes` creates two tables:
`server_processes`, the registry of live server processes, and `server_build_announcements`, each build the fleet has announced as `server.started` (Running more than one process, above).
It reads, locks and rewrites nothing that exists, and the release before it neither reads nor writes either table, so that release runs over the new schema unchanged and the census answers `0`.

Migration `0136_dashboard_assets` creates `dashboard_builds`, `dashboard_assets` and `dashboard_build_assets`, where every server process publishes its dashboard build's assets at boot and from which it answers a page of another build that asks it for that build's chunks (`internal/dispatch/dashboard`).
It creates tables and touches no row; its census answers `0`.
The release before it never reads or writes these tables, so it serves beside `0136` unaffected while both run
— its own pages are served from its own image, as before
— and rolling Dispatch back past `0136` is an image rollback alone, with no schema to restore:
the tables are left for the next release that carries them, whose boot prunes what no build still needs.

Migration `0137_request_keys` creates `request_keys`, the one record of every keyed write (Keyed writes, below), and its two indexes:
`request_keys_created_at`, for the sweep that deletes a key past its 24 hours, and the partial `request_keys_owed_sends` (`created_at`, over rows with no answer yet whose recovery point names a message), for the delivery sweep's read of a keyed send whose process died before its first attempt.
It is a new table that touches no row, which the release before it neither reads nor writes; its census answers `0`.
It follows `0129_document_edit_notices`, `0130_comments_written_version`, `0131_asks_written_version`, `0132_component_facts`, `0133_comments_written_version_backfill`, `0134_asks_written_version_backfill`, `0135_server_processes` and `0136_dashboard_assets`, which already use the preceding migration numbers.
From its release on, nothing reads or writes `broadcast_idempotency_keys` (`0055`), the broadcast's own key table before it;
it was kept through that release so a deploy of it could roll back, and `0141_drop_broadcast_idempotency_keys` drops it in a later release.

Migration `0138_pending_delivery_indexes` adds two partial indexes, `message_deliveries_pending` and `comment_deliveries_pending`, on the delivery attempts still pending, which the delivery sweep reads every minute (Keyed writes, below).
It reads, refuses and rewrites no row, and the release before it neither reads nor depends on them; its census answers `0`.
Each build holds its table's writes (a `SHARE` lock) for as long as it reads the table once.

Migration `0139_related_checks` adds three tables for the related-item check, which this release makes search by meaning first and by the project's rarest words.
`related_checks` holds one row per check of an issue, an ask or a decision block a document edit adds, written by the check itself once it has answered (a check that ran out of time, or whose request ended first, included;
the no-write related route records nothing):
its project and source, the actor, the outcome (`found`, `none` or `not_made`), how it searched (`meaning_and_keywords` or `keywords_only`, none when not made), the reason it was not made (`timeout` for the check's own allowance running out, apart from `cancelled` for the request that ran it ending first:
its client leaving for a write sent without an `Idempotency-Key`, the keyed run's own 10-minute bound for a keyed one) or searched by keywords alone, the lexemes it searched for, its keyword, meaning and total milliseconds and how many items it listed.
`search_term_stats` (how many of a project's searchable items hold each lexeme) and `search_term_totals` (how many searchable items the project has, and when they were counted) are the statistics the check picks its keywords from;
the term-statistics refresh, added with it, rebuilds a project's rows at boot and every 10 minutes, and the check only reads them.
Every row names its project with `on
delete cascade`, and a check's row also cascades from its issue, document or ask.
The tables are new, so the migration rewrites and refuses no row and its census answers `0`;
its foreign keys take the referenced tables' locks for milliseconds, as `0070`'s did.

Migration `0140_issue_embedding_spec` makes an issue's meaning-search text its title and its spec:
`issue_embedding_text(title, spec)` is the title, a blank line and the markdown of the latest version of the issue's primary document, or the title alone when that markdown is empty or only white space.
It replaces the function of `0073`'s issues trigger, which still fires on an insert or a title change, and adds a trigger on `artifact_versions` that queues the issue again when a new version of its primary document is written.
Replacing a function's body locks no table, so the migration takes `artifact_versions`' lock alone, for its trigger, as `0073`–`0077` each take one table's.
The coverage pass (Search, below) builds the same text, so an existing issue, whose row holds its title alone, is queued again by the first pass once an embedder runs and re-embedded at its title and spec;
nothing is re-embedded by the migration itself, and its census answers `0`.
While the release before it still serves, its coverage pass points those rows back at the title alone, and each pass of either release queues them again, so a roll that spans passes embeds some issues more than once;
a deployment with no embedder runs no pass and sees none of this.

Migration `0141_drop_broadcast_idempotency_keys` drops `broadcast_idempotency_keys`, the contracting half of `0137`:
a deploy that carries it can no longer roll back to a build from before `0137`, whose broadcast send writes that table.
It reads no row; dropping the table takes `ACCESS
EXCLUSIVE` on `broadcasts` too, because the table's foreign key puts triggers there, so a broadcast read or send waits for the instant the migration's transaction lasts.
Its census answers `0`.
It follows `0139_related_checks` and `0140_issue_embedding_spec`, which already use the preceding migration numbers.
Migrations `0145_events_person_words` and `0146_comment_deliveries_session` add two partial indexes for `GET /api/v1/agents/{session_id}/answers` (`sessionanswers.List`, Routes below), which every agent host reads on every model request.
`0145` creates `events_person_words` on `events
(created_at desc, id desc)` where the actor is a person and the type is `ask.answered`, `comment.created`, `message.created` or `message.answered`:
the route reads the window's human events of those types through it, newest first, and keeps those whose `published_destinations` hold the session's own topic.
A GIN index on `published_destinations` would find the session's events directly, but the outbox appends each topic it published an event to that array, and every such append would then also write the new element's entry into the index:
a write load every outbox append would pay for a read only this route makes.
`0146` creates `comment_deliveries_session` on `comment_deliveries (session_id, created_at desc)` where `session_id` is not null:
the route reads the session's mentions created since an hour before its window as one range of it.
Each is a plain `create index` inside the runner's transaction, so it holds `SHARE` on its table while it builds: writes to it wait, reads do not.
On a local Postgres 17 (two CPUs, `maintenance_work_mem` 64 MB) with `events` seeded to 1,000,000 rows (421 MB with its indexes, 126,541 of them in the index), `0145` built in 0.66-1.13 s over three runs and `0146` in 0.11-0.14 s on 50,003 deliveries.
Both censuses answer `0`: an index build refuses and rewrites no row, and the census report already counts and sizes each table it reads.

Migration `0147_message_deliveries_session_created` creates `message_deliveries_session_created` on `message_deliveries (session_id, created_at desc) include (message_id)` where `state` is `sent`, for the same route's read of the session's sent message deliveries created since an hour before its window.
`message_deliveries_session` (`0051`) keys `session_id` alone, so through it that read fetched the session's whole delivery history and filtered it by time:
a long-lived session that receives every broadcast copy read all of its deliveries on every model request.
The new index holds only the read's sent rows and carries the one column it returns, so the read is an index-only scan of the window's range, and a cached generic plan, which knows neither the session nor the window, chooses it too:
costed as a plain `(session_id, created_at desc)` index it came within a percent of `0051`, which the generic plan kept.
`0051` stays for the conversation window, which reads every delivery a session ever received.
On the seeded database above, with one session given 60,003 sent deliveries (1,019 since an hour before its window), the read of them went from 58,984 rows fetched and discarded (798 buffers, 20-22 ms) to an index-only scan of the 1,019 (11 buffers, 0.3-0.4 ms), in the custom and the generic plan alike;
the index built in 0.34-0.52 s over three runs on 110,003 deliveries.
A plain `create index` inside the runner's transaction, it holds `SHARE` on `message_deliveries` while it builds: writes to it wait, reads do not.
Its census answers `0`.

Migration `0142_item_blocks` creates the tables behind the blocks of a comment or message as graph nodes (`packages/envoy/AGENTS.md`, "References form one graph"):
`comment_blocks`, `message_blocks`, `comment_block_texts`, `comment_block_state`, `message_block_state`, `comment_block_refs` and `message_block_refs`, each with a foreign key to `comments` or `messages` that cascades, with their indexes, among them the partial `comment_block_state_owed` and `message_block_state_owed` through which the block-index reconciliation reads what is owed.
It adds the nullable `replies_to_block` to `comments` and to `messages`, a catalog change that rewrites and scans no row, with a partial index on each.
It creates the trigger function `item_blocks_owe`, which no trigger calls until `0143`, and re-states the `graph_edges` view with six more arms:
a current block's `part_of` edge to its comment or message, the links a current block holds, and a reply's `replies_to` edge to the block it names, under the storage kinds `comment_block` and `message_block`, which a build without them skips as unknown kinds.
Adding the columns holds `comments` and `messages` `ACCESS EXCLUSIVE`, and re-stating the view holds `graph_edges` `ACCESS EXCLUSIVE`, for the migration's short transaction, so every read and write of a comment or message, and every reference read, waits behind it.
It writes no row: the state rows are seeded by `0143` and `0144`, after their triggers.
It refuses and rewrites no existing row; its census answers `0`.

Migration `0143_comments_item_blocks_trigger` attaches `item_blocks_owe` to `comments`, `after
insert or update of body`: a comment any build writes is owed a block index (`comment_block_state.owed_at`), which a block-aware build clears by indexing the comment in the same transaction, so a comment a build without the index writes during the roll, or after a rollback, stays owed until the reconciliation indexes it.
In the same transaction it then owes an index to every comment already stored, reading `comments` once.
The trigger takes `SHARE ROW EXCLUSIVE` on `comments` alone until the commit, so a comment write waits behind it, while a read does not, and no comment is written between the trigger and the seed.
Its census answers `0`: the one table it writes is `0142`'s, which holds no row at deploy, and the rows it inserts are new work owed, not a rewrite.

Migration `0144_messages_item_blocks_trigger` does the same for `messages`, `after insert`, a direct message included (a message is never edited), and owes an index to every message already stored, reading `messages` once.
Its trigger takes `SHARE ROW EXCLUSIVE` on `messages` alone, so a message write waits behind it while a read does not.
Its census answers `0` for the same reasons.

Migration `0148_asks_anchor_artifact_id` creates `asks_anchor_artifact_id`, an index over the asks whose `anchor` is set of `case when anchor->>'artifact_id' ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' then (anchor->>'artifact_id')::uuid end`:
the uuid cast `comments_anchor_artifact` (`0032`) indexes, guarded by the form every server-written anchor carries, since an unguarded cast would fail the boot over an anchor an earlier release could store.
Settlement's sweep reads through it the marks a document's asks record, by the anchor's document alone, so an ask that also names an ask block on another document (`block_artifact_id`) keeps its mark on the document its anchor names.
Building it takes `SHARE` on `asks`, so ask writes wait for the build; it rewrites no row and refuses none, so its census answers `0`.
An anchor whose artifact id is not a lower-case document id is left out of the index, which costs the read nothing
— its text comparison never matched such a spelling either — and each boot logs the count of those asks with a warning when there are any.
The release before it never reads the index and serves beside it unchanged, except that its sweep still drops the mark of an ask holding both columns (no such row is written today), and rolling Dispatch back past `0148` is an image rollback alone:
the index is left for the next release that reads it.

Migration `0150_dpop_proofs` creates `dpop_proofs`, the record of every RFC 9449 DPoP proof Dispatch has accepted, and its expiry index.
A caller presenting an access token the secrets broker issued signs a fresh proof of that token's key for each request (`internal/brokertoken`), and the row keyed on the proof's `jti` and normalized `htu` is what refuses the same proof presented twice;
the maintenance pass deletes every record past its proof's acceptance window beside the idempotency-key sweep.
It is a new table that touches no row, which the release before it neither reads nor writes; its census answers `0`.

Migration `0009_project_artifacts` deletes malformed derived artifact references, reports their count, and re-derives them from source text on the next write.
It aborts server boot before a migration record or schema change only when an existing artifact has no owning issue.
On success it backfills each artifact's project and generated `ref_key`.

A migration runs while the build before it is still serving.
The app Deployment of `envoy-infra:gcp:Dispatch` rolls above one replica (`packages/envoy/infra/README.md`), so from the start of a rollout until the last pod of the previous release is gone
— and again for as long as a rollback lasts — the previous image's queries run against the new schema.
A migration may therefore only add: a table, an index, a trigger, a column that is nullable or defaulted, a constraint added `not valid`.
In the same release as the code that depends on it, it may not drop or rename a column or a table, change a column's type, or tighten a constraint the running build's queries would violate.
Each of those is split across two releases: the first makes the schema accept both shapes, and the second removes the old one once no pod serves it.

The census below does not cover this.
It reports locks, sizes and counts — what a migration would wait behind and how much it would rewrite
— not whether the previous build's queries still answer against the new schema; that one-release gap is the author's to reason about.
The five-second `pgmigrate.LockTimeout` also bites harder under a rolling update than under `Recreate`, which stops the previous pods before the new one boots:
while a deployment rolls, the old build's connections are still open and still holding locks on the tables the migration wants, so a migration that applies cleanly against an idle database can fail the new pod's boot.

### Pre-deploy census

`envoy-dispatch census` is run before a new image goes out, with the service's `DATABASE_URL` and under the service's own database credentials, and it is a gate:
a census that refuses stops the deploy before the service moves, and nothing accepts a refusal and rolls past it.
The `envoy-infra:gcp:Dispatch` component registers no census task, so a deployment runs it itself against its database before it updates the image:
as a Kubernetes Job from the new image, or by hand with the `kubectl run` recipe in `packages/envoy/infra/README.md`.

```bash
DATABASE_URL=postgres://... envoy-dispatch census
```

The census is one `repeatable read, read only` transaction over the migrations this binary carries that the database has not applied:
every migration whose version `schema_migrations` does not record, the rule the runner applies them by (a database without that table records none, so everything is pending).
For each it prints the tables the migration locks above ACCESS SHARE (`pgmigrate.CensusTables`), with each table's total size, its row count and the sessions holding locks on it;
the transactions open longer than a minute anywhere in the database, and those whose age it cannot see (another role's without `pg_read_all_stats`, any with `track_activities` off, or a WAL sender's), which it tells from a session in no transaction by the virtual transaction id every transaction locks;
and the count the migration's own census answers.
Postgres reads `pg_stat_activity` once per transaction and serves that copy until the transaction ends, while `pg_locks` is read anew each time, so before each read of lock holders and of long transactions the census discards that copy (`pg_stat_clear_snapshot()`), and it measures a transaction's age at that read (`clock_timestamp()`), not at its own start.
Those tables are:

- the ones its statements name (`pgmigrate.TouchedTables`:
  the targets of `alter table`, `create index … on`, `drop table`, `truncate`, `create trigger … on`, `update` with or without an alias, `delete from`, `insert into`, `lock table`, and a foreign key's `references`), read as Postgres's lexer reads the migration:
  not in its comments, its string literals or the body of a function it defines, and in a `DO` block's body, which it runs;

- the table behind each index it drops or alters;

- every table a foreign key reaches from a table whose rows it writes, or from a table it drops, read from `pg_constraint` when the census is taken, and from the `references` an earlier pending migration adds, which applies first at boot (in a `create table` or `alter table`, inside a `DO` block too, behind an `if not exists` over `pg_constraint` or not):
  a row inserted or updated (an upsert's `do update` included) is checked against the table its key references, a dropped table's own foreign keys go with it and take ACCESS EXCLUSIVE on each table they reference, and a row deleted, or one whose key an update changes, is checked against every table that references it or cascades into it, and on from there.
  The report prints such a table as `touches <table>
  through a foreign key with <table>`, and checks its holders and that it can be read, but neither counts its rows nor holds it to the size limit:
  the migration locks it to check or act on the rows that key connects, or, for a drop, only for the catalog change, reading no row.

A table a statement only reads (`insert … select from`, `create view … as`) is not listed: its ACCESS SHARE waits only behind an ACCESS EXCLUSIVE holder.
Each store's tests apply every one of its migrations in turn and require that reading to be the tables the migration locks above ACCESS SHARE (`pgmigratetest.CheckTouchedTablesAgainstLocks`), so a migration written in a form the patterns do not know (`reindex`, `cluster`, `create policy … on`, `merge into`) fails there and needs a pattern.
They apply the real set to an empty database, where a foreign key locks nothing, since it locks only the rows it checks or cascades into;
the audit's own tests seed rows to hold the foreign-key reading to the locks it predicts.
A table only a `DO` block's body names is not held to the other half, a table read that the migration never locks:
the block can branch on rows the empty database does not hold (`if exists (select 1 from things) then update others …`).
The census checks such a table all the same.
A fresh database, one that records no version and holds no table, has no row a migration could refuse and no session to wait on:
the census prints `census: fresh database, nothing to check` and passes, so a new stack's first deploy takes the census like every later one.
It refuses:

- a migration whose census answers non-zero, naming the migration, the count and the census file;

- a table a migration names above 1 GiB (`pgmigrate.CensusTableLimit`, table, indexes and TOAST), whose rows it then does not count:
  a migration that locks a table that size needs another shape (`CONCURRENTLY`, batches).
  A migration whose every statement is a `create function` (or procedure) in `plpgsql` or `sql`, or a `create trigger`, as Postgres's own parser reads them (libpg_query, run in pure Go by `github.com/wasilibs/go-pgquery`), is exempt:
  each does its work in the catalog and reads no row of the table, so the time it holds its lock does not grow with the table.
  A function in another language is not exempt, since creating a C function loads its library, which can run any code;
  and no migration is exempt while an enabled event trigger runs on `create function`, `create procedure` or `create trigger`, since it runs code of its own, and the size refusal then names it.
  The report still prints the table's size beside the limit and says why the migration is exempt, and its lock holders refuse it as they refuse any other.
  A trigger named only in a comment, a string literal or a function body exempts nothing, and one statement of any other kind beside it ends the exemption;

- a session holding a lock on a touched table whose transaction has been open at least a minute (`pgmigrate.CensusLongTransaction`):
  the migration would give up on that lock during the rollout;

- a session holding a lock on a touched table whose transaction's age the census cannot see.
  `pg_stat_activity` shows another role's `xact_start` and `state` only to a superuser, a member of that role or a member of `pg_read_all_stats`, so without that grant every holder of another role (an operator's session, a dump) refuses, since the census cannot tell it is young, and the refusal names the grant;
  granting the census's role `pg_read_all_stats` lets it read the age of a holder of another role, and then it judges such a holder as it judges one of its own role, and the refusal never names the grant for a holder of the census's own role.
  A session with `track_activities` off shows its state as `disabled` and no `xact_start` to every role, so it refuses too, and the refusal says so.
  Postgres never shows a WAL sender's `xact_start`, to any role, and a `replication=database` connection runs SQL, so it can hold a lock in a transaction of any age:
  a holder that shows no `xact_start` and a state other than `idle` refuses, of the census's own role too, and the refusal names its state.
  Only an `idle` holder with no `xact_start` passes:
  `pg_stat_activity` read it between transactions, in the query that read its lock, so the transaction holding that lock began or ended within that query.
  A session caught at a statement's start or end reads `active` with no `xact_start` and refuses too, spuriously and only at that instant, so the census taken again passes it;

- an autovacuum worker holding a lock on a touched table, only when Postgres will not cancel it for the migration.
  Postgres cancels an autovacuum that holds a lock another session waits for once that session has waited `deadlock_timeout`, so an autovacuum passes at any age, with the grant or without (to a role without it, autovacuum is the holder that runs as no role), except an anti-wraparound vacuum, which Postgres does not cancel, or one on a server whose `deadlock_timeout` is not shorter than a lock timeout the migration waits under:
  the runner's five seconds, or the one its own `SET LOCAL lock_timeout` sets, which Postgres reads in a savepoint of the census's (a value it refuses there refuses the migration).
  `0066` sets 500 ms, so an autovacuum on `asks` or `comments` refuses it at the default one second.
  Postgres marks a vacuum anti-wraparound when it launches it, in its activity, which only a role with `pg_read_all_stats` can read, and which Postgres records only with `track_activities` on;
  where the census cannot read it, it refuses every vacuum of a table past its freeze age (`age(relfrozenxid)` or the multixact age at its `autovacuum_freeze_max_age`), one launched before the table passed it included;

- a touched table the census cannot read within five seconds (`pgmigrate.LockTimeout`), because another session holds a lock every read waits behind, or within the statement timeout (a minute, `pgmigrate.CensusStatementTimeout`), naming the holders;

- a census that fails or answers anything but one integer in one row;

- a database that records no version yet holds tables, on its first migration: the runner would apply every migration from `0001` over them.

Every refusal a lock holder causes (an old transaction, one whose age the census cannot see, an autovacuum Postgres will not cancel, a table it could not read) carries each such holder's diagnosis, read when the census refused, since the holder has usually gone by the time anyone looks:
its state, transaction start, `backend_xid`, wait event and query from `pg_stat_activity`, and every lock it holds or waits for from `pg_locks`.
Its query is printed with every literal, number and comment replaced (read with `standard_conforming_strings` on), so the values a query carries in those do not reach the report;
what Postgres hides from the census's role prints as `not visible`.
A diagnosis that cannot be read says so with its SQLSTATE, and the refusal stands as it is.
To read the same by hand for a session `<pid>`:

```sql
select a.state, a.xact_start, a.backend_xid::text, a.wait_event_type, a.wait_event, a.query,
	l.locktype, l.mode, l.granted, l.relation::regclass::text
from pg_stat_activity a left join pg_locks l on l.pid = a.pid
where a.pid = <pid>
order by l.granted desc, l.locktype, l.relation::regclass::text, l.mode;
```

So a migration that changes values readers already had, whose census counts the rows it rewrites, cannot pass the census wherever such a row exists, and a release that must repair existing rows does so in code instead:
an idempotent pass at boot, and on the path that reads the rows, so a task of the earlier release still running during the roll cannot undo it.
The restoration of decided asks' wordings (`docs.Service.RestoreDecidedWordings`, Database migrations above) is one.
The repair of closed and reopened issues' asks (`api.RepairIssueAsks`, Database migrations above) is the exception:
it repairs at boot and then every 10 minutes (`api.IssueAskRepairInterval`), never on a read, because those rows are read by every issue, ask, Inbox and count route, and a read that repaired them would put a close's document writes on all of them.
During a roll a close or reopen by a process of the earlier release stays unconverged for at most one interval and one pass, and the Inbox, `GET /api/v1/asks/open` and the counts keep leaving out the open asks of a closed issue to cover that window.

A migration declares its census in `<version>_<name>.census.sql` beside it:
one `select` (or `with … select`) answering one integer, how many existing rows the migration would refuse (a validating constraint's violators) or rewrite.
Every migration from `censusRequiredFrom` (`internal/dispatch/store/store_test.go`, 56) on declares one, which from now on is every migration added:
a timestamped version is above every number.
one that cannot refuse or rewrite a row says so with `select 0` and a comment (`0054_message_delivery_acceptance.census.sql`;
`0053_asks_approval_kind_check.census.sql`, the check's own predicate negated, is the other shape).
A census is written against the schema just before its migration, and the store's tests run every shipped census at exactly that schema and require it to answer (`TestEveryShippedCensusAnswersAtTheSchemaBeforeItsMigration`).
A deployment takes every census before any migration of its release applies, so a census names only what exists before its release.
One that names a table or column the database does not have refuses, even behind an earlier pending migration that may be what creates it:
the census cannot tell that from a typo without applying that migration, which takes the very locks it measures.
A release whose census reads what an earlier migration of the same release creates, renames or gives a new type therefore refuses every deploy, until a release carrying the earlier migration without the later one deploys first.
The store's tests find such a pair before it ships (`TestEveryShippedCensusNamesTheNewMigrationsItReads`, `pgmigratetest.CheckCensusNamesTheMigrationsItReads`):
a census that reads a table, column, function or type a migration from `censusRequiredFrom` on creates, renames or, for a column, gives a new type names that migration in a comment saying it ships in an earlier release.
A rename or a new type keeps the catalog row, so the check compares every object's name, and every column's type, before and after each migration.
Where releases split is not in the repository, so the comment is the author's word;
when both would ship together, split the release, or write the census without what the earlier one makes, which for a new table or column holds no row at deploy (`select 0` with a comment).
The runner never runs a census.
Exit 0 passes, 1 refuses (every reason is in the report), 2 the census could not be taken (no `DATABASE_URL`, no connection, a `search_path` naming no schema that exists, where `current_schema()` is null and the runner can create nothing, a migration set `pgmigrate.Load` refuses).

**A census is production-executed code, reviewed like the migration beside it.**
It runs as the service's own database role.
The read-only transaction stops every write, and the census's statement is sent through the extended protocol whatever the connection string asks for, so it cannot carry a second statement (a `commit` of its own, then a write).
Read-only does not stop `pg_terminate_backend` (which, as that role, ends the service's own sessions), `pg_cancel_backend`, advisory locks (`pg_try_advisory_*` included) or `pg_sleep`, nor a function that runs a query given as text (`query_to_xml` and its kin, `ts_stat`, `ts_rewrite`):
`pgmigrate.Load` refuses a census naming any of them, bare or quoted and in any case, anywhere outside its comments and string literals, and refuses a Unicode escape (`U&"…"`, `U&'…'`) outright, since one can spell any name and no census needs it.
`Load` finds the comments and literals as Postgres 18's lexer does (nested comments, `E'…'` escapes, continued and dollar-quoted literals), and the census runs with `standard_conforming_strings` on, so Postgres reads its literals the same way.
Review is the control past that, since any non-zero integer a census answers is printed.

**What the report prints:** counts, sizes (in KiB, MiB and GiB, or in bytes where a size above the limit would round to the limit's figure), versions, file names and session metadata (pid, role, application name, state, transaction age as Postgres measured it, to the microsecond, the figure the minute's bound is compared with;
never query text, "not visible" for a field Postgres hides, and "autovacuum worker" for one).
For a census that fails, its file name and the SQLSTATE;
Postgres's own message only when the error points into the census's own text (a position, which a parse or analysis error carries), since then it quotes that text and its identifiers.
An error raised while the census runs carries none, and its message is never printed:
a data exception's quotes the row value that failed to cast, and a `reg*` cast's (`regclass`, `regtype`, `regproc`) quotes the row's text under a class-42 SQLSTATE;
the advice about an earlier migration creating a missing name is given only for a name the census's own text spells.
The census writes nothing.
An argument `envoy-dispatch` does not know is refused with exit 2, never served, so an image that predates a subcommand cannot boot and migrate when a deployment asks it for one.

## Reference graph

Mentions (`dispatch://` references and same-origin dashboard URLs) in document versions, ask questions, comment bodies, and issue messages are indexed into `refs` on every write;
migration `0032_refs_provenance` gives each edge a `kind`, `created_at`, and `source_seq` (the `events.id` that introduced it), and creates the `graph_edges` view, which unions those mentions with the structural relations already stored on the node tables (`child_of`, `attached_to`, `anchored_to`, `owned_by`, `replies_to`, `followed_by`).
`GET /api/v1/references?to=<ref>` lists everything that points at a node and `?from=<ref>` everything it points to, cross-project.
A row whose other end is a node kind the running build does not know (a newer build's, read during a rolling deploy) is left out of every list and every count that build serves (`nodeLoaders`, `refs/graph.go`, names the kinds it knows).
The migration refuses to run when an ask or comment anchor carries a non-uuid `artifact_id`.

Each block of a document is a node of the graph too (migration `0121`; `packages/envoy/AGENTS.md`, "References form one graph").
Its reference is the document's with `#b-` and the block's id, percent-encoded as `encodeBlockId` (`@legion/contracts`) writes it:
`dispatch://KEY/spec#b-<id>`, `dispatch://KEY/artifact/<slug>#b-<id>` or `dispatch://PROJECT/artifact/<slug>#b-<id>`.
Text cites a block only while its id holds at most 256 bytes decoded (`text.MaxBlockIDBytes`), and any node only while its target id, the id its edge is stored under (`text.TargetID`:
an issue's key, a document's `<KEY or PROJECT>/<slug>`, a document block's `<KEY or PROJECT>/<slug>#<id>`, a comment's or message's block's `<item kind>/<item id>#<id>`, an ask's, a comment's or a message's id), holds at most 2048 bytes (`text.MaxTargetBytes`), since `refs` and `block_refs` hold it in B-tree keys and neither a slug nor an item id has a bound of its own:
text citing past either is written with no edge.
The bounds decide only what the graph indexes.
Every write is taken whatever the length of a block id it names (a seeded spec, an upload, an `insert`, a rebuild, an accepted suggestion, a browser's edit);
a block carrying an id past 256 bytes is no node, and the server logs a warning naming the document, the longest id's length and the bound, and writes the version.
A route reads the reference it is given whatever its length (`text.ParseRef`):
`?to=`, `?from=` and `GET /api/v1/block-links` read a node of any name, and a block whose id is past 256 bytes is `404 NOT_FOUND`, no node.
An ask block and an anchor in a block are written under an id of any length (migrations `0127` and `0128`, Database migrations above).
A mention written inside a block starts at the innermost block holding it, so `?to=` a cited node names that block, whose `document` is the citing document;
a document's own read also lists the links at its blocks, each edge naming its block as `block`, and `?kind=part_of` on a document lists each of its current blocks.
A block reference pinned to a version of its document, `#b-<id>@v<N>` with N at most 9223372036854775807, reads the block as that version held it:
the node's `pin` carries the block's text at `N` and says whether the block is `unchanged`, `changed` or `removed` since.
`GET /api/v1/block-links?from=<document>&to=<document>` lists the links from the blocks of one document to the blocks of another.
Text stored before the release, and text an earlier build writes during the roll, is indexed by the server's block-index reconciliation (lease `block-index`), with no command run.

A comment and a message are blocks too (migrations `0142`-`0144`; `packages/envoy/AGENTS.md`, "References form one graph").
The server splits a body into its top-level blocks as the document parser reads them, each block the text as its author wrote it (a body the parser refuses, such as one holding block HTML, splits at blank lines into blocks of type `text`;
a body of only whitespace has none), and names them `b1`, `b2`, …, never minting a number twice in one item.
A block's reference is its item's with `#b-` and the id:
`dispatch://KEY/comment/<id>#b-<id>`, `dispatch://PROJECT/artifact/<slug>/comment/<id>#b-<id>` or `dispatch://KEY/message/<id>#b-<id>`, and a comment's or message's dashboard page with a `#b-` fragment names it too.
Its graph node is kind `block`, id `comment/<uuid>#<id>` or `message/<uuid>#<id>`, whose `parent` is the comment or message;
`graph_edges` stores it under the kinds `comment_block` and `message_block`, which a build without item blocks skips as unknown kinds.
A mention in a comment or issue message starts at the block holding it, so `?to=` a cited node names that block and counts each citing block once;
a comment's or message's own read lists the links at its blocks, each naming its block as `block`.
A direct message writes no mention.
An edit of a comment keeps each block's id:
with the ids the dashboard editor carried (`blocks` and `base_revision` on `PATCH /api/v1/comments/{id}`), a block keeps the id whose span starts in it, so a moved, reworded, split (the part holding the start), joined (the first) or restored block keeps it;
without them, or against a revision another save replaced, ids follow the text.
Each indexed edit of a comment is a revision, and a comment block reference pinned to revision N, `#b-<id>@v<N>`, reads the block as N held it, with `pin` saying `unchanged`, `changed` or `removed` since;
a message never changes, so a message block takes no pin (`400 INVALID_REFERENCE` on a route, no reference in text).
A reply names the block it answers with `reply_to_block` (a comment) or `in_reply_to_block` (a message), a `replies_to` edge to that block, and every comment and message a route or event returns carries `reply_block`, `null` when it names none.
`GET /api/v1/comments/{id}/blocks` and `GET /api/v1/messages/{id}/blocks` list an item's current blocks.
A comment or message an earlier build writes during the roll, or after a rollback, is owed its blocks (`owed_at`, set by the triggers of `0143` and `0144`), which the same block-index reconciliation indexes, at most 200 a tick, with no command run.

```bash
DATABASE_URL=postgres://... envoy-dispatch rebuild-refs
```

reparses every source and reconciles the index with the text (surviving edges keep their provenance, edges of deleted sources are removed), marks every document owed a block index and every comment and message owed a block index of its own blocks, which the server's block-index reconciliation then indexes, and prints `rebuild-refs: documents=N asks=N comments=N messages=N orphans=N edges=N blocks_owed=N items_owed=N`, `blocks_owed` being the documents it marked and `items_owed` the comments and messages.
It reads `dispatch.server_url` from the envoy config the same way the server does and refuses an empty value:
dashboard-URL mentions are recognised only against it, so an empty URL would drop them all.

A picture in a message, comment, reply, ask question, answer or resolution reason is CommonMark's image syntax whose address is the picture's own Dispatch reference, versioned:
`![<file name>](dispatch://<owner>/artifact/<slug>@v<N>)`, where the owner is an issue key, a project key, or `agent/<session id>` for an agent's conversation.
The reference inside is indexed as any mention is.
An agent's conversation's file is `dispatch://agent/<session id>/artifact/<slug>[@vN]`, the session id as written, and its dashboard page `/agents/<session id>/artifacts/<slug>[?v=N]` names the same artifact;
a session id holding a `/`, `?`, `#` or whitespace names nothing.
`graph_edges` has no structural edge for such a file (no `attached_to`, since it belongs to no issue):
it appears in the graph through the mentions that cite it.

Every node the graph reads carries six facts, whatever its kind:
`made_by` and `made_at` (who made it and when), `changed_by` and `changed_at` (who last changed its own text or fields and when, its maker while nothing has changed it since), `parent` (`{kind, id, ref?}`, the node it belongs to, `id` that node's own id) and `parent_version` (the version of that parent it belongs to:
for a document's block its changed version, for a comment's block its changed revision;
for an ask or comment written on a document the version its anchor names, for a block ask the version that first held its block, for an approval request the version it named when it opened, else the version current when it was written).
A reply that names a block is part of that block.
A fact Dispatch never recorded is `[]` for an actor list and `null` otherwise:
a session has none, a component's makers are never recorded and its times only from this release on, a document block's maker, and a block ask's version, are known only when the block is in the document's first version or Dispatch indexed the version before the one that first held it, and a comment block's making time is not known for a block found at the first indexing of a comment already edited by then (`packages/envoy/AGENTS.md`, "References form one graph", gives each kind's sources and what counts as a change).
Migrations `0130`-`0134` (Database migrations above) add what the facts read:
the version current when an unanchored comment or ask is written on a document, recorded at insert and filled for the rows already stored, and `component_facts`, which every import keeps.
`dispatch read` prints the facts of the node it reads on a `Node:` line, before `Referenced by:`.

## Search

Migration 0010 added stored generated `search` columns.
Migrations `0057`–`0061` made them plain columns that a `BEFORE INSERT OR UPDATE` trigger per table fills (`issues_search`, `artifact_versions_search`, `comments_search`, `asks_search`, `messages_search`), so no application code writes or refreshes a search vector.
Every indexed text, headline, and duplicate-title comparison first passes through `search_text` (0056), which puts a space after every sixteenth underscore-joined segment.
A query is not normalised and is limited to 1,000 characters.
Search covers issue titles, the latest settled document text, comments, asks (questions, option labels and descriptions, free-text answers and the options an answer chose), and messages.
An ask hit's excerpt is cut from that same indexed text (the question, then `ask_search_suffix`), never from the options' raw JSON, and the hit carries `ask`, where the ask stands when the search ran:
`open`, `answered` with its latest answer, or `resolved` with its resolution and, when anyone answered it, the last answer it had, which for an ask that document settlement retracted while it was answered is read from its `ask.answered` events, since its row keeps none.
Live document text takes up to the 2 s settle delay to appear in search results.

Every trigger builds its vector with `search_vector` (0087; the issues trigger's from 0088).
Postgres holds at most 1,048,575 bytes of lexemes and positions in one vector.
Prose stays far below that, since its words repeat, but text of words no two alike passes it well inside a document's 1 MiB:
about 700 KB of `w000001 w000002 …`, or 475 KB of UUIDs.
`search_vector` indexes a text whole when its vector fits, and otherwise the longest of its first half, quarter, eighth, … that fits, cut between two words, so keyword search finds such a document, comment, ask or message by the whole words in that opening part and not by the words after it;
a text with no whitespace before the cut is found by none of its own.
An issue's key is always indexed whole.
Meaning search, where an embedder is configured (below), ranks by an embedding of a text's first 32,000 characters (`embed.maxInputChars`, which both clients cut at), not by its vector, so it can still return such a text for a query whose words the vector does not hold.

Meaning search embeds with Cohere Embed v4 at the place `DISPATCH_EMBEDDER` names:
AWS Bedrock (`bedrock`, the default, on the AWS SDK's own region and credentials) or Azure AI Foundry (`azure`, the `DISPATCH_AZURE_*` settings, with the deployment's API key or a keyless sign-in that exchanges a federated token with Microsoft Entra).
Bedrock with no AWS region logs `dispatch: meaning search
unavailable` and leaves meaning search off;
then, and whenever the query's embedding fails, a search answers by keyword alone with `degraded: embedder_unavailable`.
Each vector records its embedder's identity in `embeddings.model`, `us.cohere.embed-v4:0` on Bedrock and `azure:<endpoint host>/<deployment>` on Azure, and search compares a query vector only with stored vectors of its own identity.
A write enqueues its text through its table's trigger (0073–0077) in the same transaction.
An issue's text is its title and its spec, the latest version of its primary document (its title alone while that is blank), and a new version of the spec queues the issue again through `0140`'s trigger on `artifact_versions`, so meaning search, and the related-item check below, find an issue with a terse title by what its spec says.
The server also runs a coverage pass (`embedqueue.Cover`) once when it starts, so a deployment runs nothing by hand to embed what it already holds:
the pass adds a row for every searchable item that has none (one written before the triggers existed, or with them off, as a restore or a replication writes), points a row whose item's text changed with the triggers off at the current text, takes every vector another identity made out of the index and queues its item again, gives a row that died under another identity this identity's attempts once, and settles a dead row whose text has nothing to embed (empty or only white space) with no vector, as the queue settles a pending one.
The two steps that act on another identity's rows wait until the configured embedder has answered:
an embedding this process's queue made has succeeded, or, when none has yet and such rows exist, the pass embeds a short fixed canary text and it succeeds.
Until then the pass leaves those vectors and dead rows as they are and logs one WARN naming the identity, the canary's error and how many of each it left, so a misnamed Azure deployment (which answers `404 DeploymentNotFound`) or a refused credential drops nothing, and a throttled canary waits too;
a pass with no such rows calls the embedder not at all.
After the boot pass only those two steps run again (Background loops, above):
at once when this process's queue first embeds successfully after the pass skipped them, and every `identityRecheckInterval` (10 minutes, plus up to a tenth again) while a run still finds another identity's rows, a canary first when the queue has not proved the embedder, until a run finds none.
The rest of the pass runs again only at the next start:
a live write enqueues its own text through its trigger, and what is written with the triggers off waits for that start.
Each step is a statement of at most 500 rows in its own short transaction, repeated until it changes nothing, and is safe beside a live write and beside another process's pass, so the pass takes no lease.
`GET /api/v1/search/coverage` reports what that leaves:
the identity, its place and model (all three null with no embedder), and in total and per kind the searchable items, how many carry a current vector of that identity, how many lack one, how many failed for good, and how long the oldest lacking item has waited.
The `backfill-embeddings` subcommand, which did the pass's first job by hand, is gone, with its checkpoint table (0119).

Every process of one deployment runs the same embedder configuration, since each one's pass drops the vectors any other identity made.
Moving a deployment to another place or Azure deployment makes every stored vector another identity's:
search finds by meaning only what the new identity has embedded, and the coverage route counts the rest as lacking, until the pass and the queue have re-embedded it at the place's background pace;
a new place that cannot embed leaves the old vectors in the database, unused by search, until it can.
A model change on Azure takes a new deployment name, since the identity names the deployment, not the model behind it.

Each kind of content is ranked on a list of its own, by `ts_rank_cd`, then recency, then id, and the five lists are merged by reciprocal rank fusion:
a hit at position p of its list scores 1/(60 + p), which is its `rank`.
The page therefore takes each kind's best in turn, the best issue, document, ask, comment and message first, rather than a run of long documents that repeat the query's words.
Hits that score alike are ordered issue, document, ask, comment, message, then by recency and id, so the order is total and consecutive `offset`s walk it once while the corpus holds still.
An issue whose key is the whole query heads the issue list.
Each list keeps its best `contracts.SearchKindDepth` (100) hits:
the answer's `total` counts every match of every kind, and `reachable` the hits the pages can return, so an `offset` at or past `reachable` answers no hits with both counts.

Search snippets are escaped text with only server-inserted `<mark>` elements around matches.
Native issue creation rejects a title that near-duplicates an existing issue in the same project with `409 POSSIBLE_DUPLICATE` and up to five candidates;
`force` bypasses that check, and external references skip it.
An issue title is at most 1,000 characters (`contracts.IssueTitleMax`, UTF-16 units after trimming), on creation and on a retitle, and a longer one is `400 CAP_EXCEEDED` before the duplicate check runs.
The check compares the new title's lexemes with those of every other title in the project, read from `issues.title_lexemes`, which the issues trigger fills with `title_lexemes(title)` (0088), the function the check calls for the new title, so a creation parses one title;
a candidate's snippet marks the words its title shares with the new one, and the `POSSIBLE_DUPLICATE` message quotes a candidate's whole title.
The cap bounds what one stored title adds to every later creation in its project, and how long that message can be:
beside 2,000 stored titles at the cap, a check takes 56–95 ms, where parsing every title as it ran took 2.1–7.6 s.

`POST /api/v1/issues`, `POST /api/v1/issues/{key}/asks` and `POST /api/v1/artifacts/{id}/asks` run the related-item check once the write has committed, and so does each decision block a document edit (`POST /api/v1/artifacts/{id}/edits`) adds, on the block's question (the check replaced LEGION-550's 300 ms keyword suggestions).
The check never refuses the write, but the response waits for it, at most 5 s (`relatedCheckAllowance`);
an edit's blocks are checked together under that one bound, the first five of them, and each block past five is answered as not made.
It searches the write's own project in two stages started together:

- Stage 1 picks the text's rarest words and runs search's keyword query over them.
  Each lexeme is scored by its count in the text times its idf, BM25's `ln(1 + (N − df + 0.5)/(df + 0.5))`, where df is how many of the project's searchable items hold it and N how many items the project has.
  At most 25 are kept, highest score first;
  a lexeme no other item holds, one held by more than a tenth of the project's items and more than 10, a number and a hash are dropped, and a word whose df would take the chosen words' summed df past 3,000 is skipped for the next by score, which bounds how many rows the OR of them matches and so its time.
  The query's keyword arms are ranked by idf-weighted coordination, the sum of the idf of the chosen words an item holds plus a thousandth of `ts_rank_cd` to order items holding the same words, where search ranks them by `ts_rank_cd` alone, so an item holding three of the text's rare words outranks one repeating one of them.
  The rest of the query, the per-kind lists and their fusion, is search's own.

- Stage 2, when an embedder is configured, embeds the text and runs search's meaning query, which fuses the same keyword arms with the nearest vectors.
  If it answers within 3 s of the check's start, its result is the check's (searched by meaning and keywords);
  otherwise stage 1's is (searched by keywords only, with the reason).
  If stage 1 has not answered when the 5 s end, the check was not made, for time (reason `timeout`);
  if the request that ran it ends first (its client leaving, for a write sent without an `Idempotency-Key`;
  for a keyed write, which no client's leaving ends, the keyed run's own 10-minute bound), it was not made, cancelled (reason `cancelled`), which says nothing of the search's time.

df and N come from `search_term_stats` and `search_term_totals` (`0139`), which the term-statistics refresh (`api.RunSearchTermStatsRefresh`, lease `search-term-stats`) rebuilds for every project at boot and every 10 minutes; the check only reads them.
A word written since the last refresh is counted live, at most 50 per check, and a project with no statistics yet starts a refresh of its own in the background and has every word counted live meanwhile.

The result, under `advice.suggestions` (for a decision block, in `advice.decision_block_checks`, one `{block_id, question, suggestions}` per block in document order), always says what the check did.
`outcome` is `found`, `none` or `not_made`;
`search` is `meaning_and_keywords` or `keywords_only`, with `search_note` saying why keywords only, and is absent when the check was not made;
`missing` is the reason exactly when it was not made (the text holds no word to search for, the search behind it did not answer within 5s, the request that ran it ended before the check finished, the search behind it failed, …), so a client built before `outcome` still says so;
and `search_query` is two or three of the words it used, spelled as the title spells them, for a reader's own `dispatch search`.
`related` holds the three items most like what was filed, one per issue or document, each ask item carrying `ask`, where it stands (`open`;
`answered`, with its latest answer, who gave it, when and the options chosen;
or `resolved`, with the resolution and the last answer a person gave before it, if any);
`decision` names an already-answered ask that may settle the same question, with who answered and when.
An issue create forced past the title-overlap refusal also names those issues in `title_matches`.
The search leaves out everything the write's owner holds
— the new issue and its spec, or the issue or project document an ask or a block sits on and everything inside it
— so nothing is ever suggested that the writer already has.
A hit on an issue that is `done` ranks below every hit on an open owner, since keyword search otherwise puts an issue already closed as a duplicate above the open issue it was closed into.
`GET` and `POST /api/v1/projects/{key}/related` (`dispatch related`) run the same check on a title and a text and write nothing.

Every check of a write is recorded in `related_checks` (`0139`):
its outcome, how it searched, the reason, the words, and its keyword, meaning and total milliseconds.
`GET /api/v1/related-checks` counts them by source, outcome, search mode and reason, with the keyword and total times' p50, p95, maximum and how many took more than 300 ms, within `[from, to)` and, when given, one project and one source kind:

```bash
curl -sf -H "Authorization: Bearer $(cat "$DISPATCH_TOKEN_FILE")" "$DISPATCH_URL/api/v1/related-checks?project=<KEY>&from=<time>&to=<time>"
```

Every suggestion an issue's or an ask's check offers is recorded in `write_suggestions`, whose `outcome` the server's outcome sweep (`api.RunSuggestionOutcomeSweep`) advances from `ignored` to `acted_on` (the suggested item was cited from the source, or the suggested issue was updated directly) or `overridden` (the source got further activity instead)
— a count of how often the suggestion was right, without refusing anything on its strength.
The sweep is woken by event commits and judges a suggestion once the events that can resolve it are a minute old, reading only the events written since its last pass (Background loops, above);
a pending suggestion nothing writes to costs it nothing.
A ranking or latency change is measured on a restored copy of the production corpus:
`packages/envoy/scripts/corpus-copy.sh` restores one, `search-smoke.sh` prints what a searcher sees for a list of queries from any build, and `search-latency-compare.sh` runs `TestSearchLatencyOnCorpus` from a base checkout and this one alternately and gates on the difference (`packages/envoy/scripts/README.md`, "Measuring search on a copy of the corpus").

## Keyed writes

Every `POST` and `PATCH` route takes an optional `Idempotency-Key` request header naming the intent behind the write, the same on every retry of it and new for every new one (the IETF `Idempotency-Key` draft; Google AIP-155).
The dashboard mints one per press and the `dispatch` CLI one per command's write, and both retry under it by one rule (`retriesUnderKey` in `@legion/contracts`):
a failed fetch, an attempt silent too long, a 5xx, a 408, a 429 and a `409 KEY_IN_FLIGHT`.
No route requires the header: a write without one runs as it always did, unkeyed, so a script or the Legion daemon's outbox keeps working.
`PUT` and `DELETE` take no key, being idempotent already.
A key is 1 to 128 letters, digits, `.`, `_`, `:` and `-`, bare or double-quoted, and belongs to the caller who sent it:
a person's canonical login (from the dashboard or through a personal token), a service token's subject, or the shared agent token.
Dispatch keeps each key's answer for 24 hours (`request_keys`, migration `0137`; the maintenance sweep deletes older rows, Background loops above).

A keyed write finishes on the server whatever its client does.
Its handler runs with the client's cancellation taken off, bounded at ten minutes (past which its work rolls back and the key is released, as for a 5xx) and counted in the server's background wait, so a shutdown lets it finish within its 30 s stop grace;
one still running past that dies with the process, and the lease handover below takes its key over once its lease lapses.
While it runs it renews its key's 60 s lease every 20 s, until its ten minutes are up, so a client that gave up and asks again is answered `409 KEY_IN_FLIGHT` for as long as the first run lives, then that run's stored answer;
only a run whose process died is taken over, once its lease lapses.
A write Dispatch takes longer to finish than its client waits still lands once.
A repeat while the first still runs is never spooled, so a retried upload is never written to disk while its first attempt runs:
its body is read off the connection and discarded, hashed on the way, and once it has arrived the key is read again and judged by that digest.
The first still running is `KEY_IN_FLIGHT`;
a different body is `KEY_REUSED`, whether the first has answered or not;
a first that answered meanwhile is replayed;
one that committed and lost its answer is `KEY_COMMITTED`;
and one that gave the key up meanwhile is `KEY_IN_FLIGHT` with `Retry-After: 1`, whose retry runs the request.
An `Expect: 100-continue` repeat is `KEY_IN_FLIGHT` at once, with nothing read.
The key is claimed, and its answer recorded, on a context the client's leaving does not end, bounded at 10 s.

Every other answer Dispatch gives before it has read a request's body - `KEY_INVALID`, the closed-issue upload refusal, `LOGIN_CHANGED`, a refused path parameter, a refusal before decoding - is written only once the rest of the body has been read and discarded, never spooled, up to the route's limit (26 MiB on the upload routes, 1 MiB on every other).
A browser that sends a whole upload before it reads the answer (WebKit, Firefox) otherwise met the connection's close as a reset and never saw the answer.
A body past the limit is answered `413 CAP_EXCEEDED` (uploads) or `413 REQUEST_TOO_LARGE` in that answer's place, and the connection closes;
a `401` is the one early answer that stands, since a caller nobody resolves is refused before any limit is judged.
Both reads wait at most a minute for a body: one that stops arriving is given its answer with `Connection: close`.
An `Expect: 100-continue` request answered early is never asked for its body: Dispatch sends no `100 Continue`, sends the answer and closes the connection.

Each item below names a status and code, then its meaning.

- The first answer, with `Idempotency-Replayed: ?1`

  A repeat under a key whose first request finished, with the same method, route, path values, query, media type and body:
  the stored status, every header the first answer set and its body, and nothing runs again.

- The first status with `ANSWER_NOT_KEPT`

  That repeat, when the first answer was larger than the 1 MiB a key keeps: the write ran, and the caller reads what it wrote again.

- `409 KEY_IN_FLIGHT`, `Retry-After: 1`

  A repeat while the key's first request still runs (its lease renewed every 20 s while it does); nothing ran, and the caller asks again under the same key.
  A repeat once the lease has lapsed, 60 s after the last renewal, takes over a key whose holder died before it committed anything, and runs the request.

- `409 KEY_COMMITTED`, no `Retry-After`

  A repeat whose first request committed a write but whose answer was lost:
  its lease lapsed in flight, or it ended in a 5xx or a panic after committing (every transaction a keyed request commits that wrote a row marks the key's `committed_at` in that same transaction;
  a lock-only `SELECT … FOR UPDATE` marks nothing, and a COMMIT refused after the mark leaves none).
  The write landed and is not run again; there is no answer to replay, so the caller reads what it wrote.
  Also a repeat of a keyed `POST /api/v1/me/agent-tokens` that minted, once that first request has finished, answer lost or not:
  the mint's answer carries the plaintext token, so it is never kept under the key, never replayed, and no second token is minted (a mint refused with a 4xx commits nothing, so its key is released and a repeat runs it again).
  That is the one route whose answer is not stored.

- `422 KEY_REUSED`

  The key was already used for a different request; the message names its method and path, and this request did not run.
  A changed request needs a new key.

- `400 KEY_INVALID`

  The header is not 1 to 128 of those characters, or was sent twice.

- `409 LOGIN_CHANGED`

  The write names its presser in `Dispatch-Login` (every write the page makes does, `POST`, `PATCH`, `PUT` and `DELETE` alike) and its caller is someone else, or no person at all (any bearer):
  nothing ran and the key was not read, so it is untouched.
  A caller that resolves to nobody (a lapsed cookie) gets the route's own `401` instead, and a write without the header is not checked.
  The page drops every write that login pressed.

- `400 REQUEST_BODY_UNREADABLE`

  The client did not finish sending the body the layer reads before the handler; nothing ran and the key was not claimed, so the same key may be sent again.

A 5xx, a 401, a 408 and a 429 are not stored:
the key is released and the next attempt runs the request again, unless the request had already committed a write, which makes that next attempt `409 KEY_COMMITTED` at once.
Four kinds of write keep their own recovery point beside the key, so a repeat of `POST /api/v1/agents/{session_id}/messages`, `POST /api/v1/broadcasts`, `POST /api/v1/issues/{key}/messages`, or a comment (`POST /api/v1/issues/{key}/comments`, `POST /api/v1/artifacts/{id}/comments`) answers what it made as it stands now, under the first's `201`, however the first request ended, and puts on its way whatever delivery a dead process left owed;
a broadcast's repeat answers at once and sends what it owes behind the answer.
A comment's or an issue message's replay reads every member fresh and keeps from the first answer only its `advice` and `ask_waiting_on`, and only when that answer was a 2xx.
Neither send route takes a body `idempotency_key` any more.

A delivery a dead process left owed is made whether anyone asks again or not.
Every write that delivers to sessions claims each attempt before its send and settles it after, on a context its client's cancellation does not reach.
The maintenance loop's pass, once a minute, first settles `failed`, with its receipt, every owed attempt nothing can send:
a message attempt, or a keyed send with no attempt yet, on an issue that has closed (`not sent: the issue closed before this
attempt was delivered`), a mention attempt whose issue closed, whose document is no longer a project's (`not sent: the document is gone`) or whose comment no longer holds the mention (`not
sent: the comment no longer mentions this target`), each checked again under its owner's lock so an issue reopened meanwhile keeps its attempt owed.
It then sends each attempt that is still pending under a claim that has lapsed, is its message's or mention's latest, and was opened over a minute ago and inside the 72-hour duplicate window (`DELIVERY_DUPLICATE_WINDOW_MS`), the exact attempt it found under its own attempt number, so the listener drops a frame that did land;
and the first attempt of a keyed send whose process died between committing its message and opening that attempt.
An attempt that cannot be sent never takes a place in a send batch.
A send to one session answers once its own attempt has settled, which keeps a person's sends to one session in the order they pressed them;
a broadcast answers once its messages are committed, with its deliveries behind the answer.

The layer reads a keyed body before its handler runs, spooling at most the route's own limit:
26 MiB for the three upload routes (`POST /api/v1/issues/{key}/artifacts`, `/projects/{key}/artifacts`, `/agents/{session_id}/artifacts`), 1 MiB for every other.
A body past it runs unkeyed and its route refuses it.
A body the client did not finish sending is `400 REQUEST_BODY_UNREADABLE`;
one Dispatch cannot spool to its temporary directory is Dispatch's own failure, a 5xx with the key echoed, which the client asks again.

Every answer the layer judged echoes the key back (`Idempotency-Key: <key>`) - not the `400 KEY_INVALID`, a request whose caller it could not resolve, or one whose body is past its route's limit, each of which runs unkeyed - and `/healthz` answers `idempotency_keys: true` on every build that judges the header - a fact of the binary, so it is true whatever the database or NATS answer, on a `503` too.
A client tells a Dispatch that runs this layer from one that ignores the header, and would run a retried write a second time, by one or the other:
the agents' client resends a failed write only against a Dispatch whose answer carried the echo in the last five minutes, or whose `/healthz` said so in that time (`packages/envoy-client/README.md`).

## Access log

Dispatch writes one line per served request, always, with no setting to turn it off.
It is written at INFO when the request ends, through the process's own logger:

```
time=2026-10-25T05:55:15.446Z level=INFO msg="dispatch: request" method=POST route=/api/v1/issues path=/api/v1/issues caller_kind=session caller=session-0123456789abcdef address=203.0.113.9 user_agent=dispatch-cli/1.0 status=201 elapsed_ms=10 bytes=412 credential=shared-token
```

Each item below names a field of the line, then what it holds.

- `method`

  The request's HTTP method.

- `route`

  The route's own pattern (`/api/v1/issues/{key}`), so one route's requests group together whatever ids their paths carry;
  `unmatched` for a path under `/api`, `/v1`, `/auth`, `/ws` or `/healthz` that no route answers.

- `path`

  The request path as it was sent, still escaped (`a%2Fb` stays `a%2Fb`), without its query string.

- `caller_kind`

  How the caller authenticated: `user` (a signed-in person), `session` (the session a bearer named in its `actor`, or the one a broker-issued token proved), `token` (a personal agent token with no session named), `service` (a verified service-account token), `broker-token` (an access token the secrets broker issued, on a line no session was named on), `shared-token` (`DISPATCH_AGENT_TOKEN` with no session named), or `anonymous` (a public route, a request whose credential was refused, or an `unmatched` path).

- `caller`

  Who that is: the person's lowercase email, the session id, the personal token's owner, the service-account subject, or the person a broker-issued token acts for.
  `shared-token` and `anonymous` name nobody.

- `address`

  The rightmost `X-Forwarded-For` entry, the address the load balancer in front of Dispatch saw, or the connection's own address when there is none.

- `peer`

  The connection's own address without its port: the hop that reached Dispatch.
  Unlike `address`, a caller cannot forge it, so a caller that reaches Dispatch directly is still named.

- `user_agent`

  The request's `User-Agent`.
  With `address`, it tells apart callers that share one credential.

- `status`

  The response status.
  A handler that panicked before writing is logged as `500`; the client gets a closed connection, not a status.

- `elapsed_ms`

  How long the request took, in milliseconds.
  A websocket's line is written when its connection ends, so it carries the connection's whole life.

- `bytes`

  The response body the handler wrote.
  A websocket upgrade is `101` with `0` bytes: its frames are written on the connection it took.

- `credential`

  Only on a `session` line: the credential behind the session.
  `token:<owner>`, `service:<subject>`, `shared-token` or `broker-token:<person>`.
  For every one but the last that session id is the caller's own text; under a broker-issued token the credential itself named it.

- `aborted`

  Only when the handler panicked (an artifact download whose store fails mid-stream aborts this way): `true`.
  The line is still written, and the panic still reaches the server.

The line never carries a token, a cookie, a query string, a request body or a response body.
The text in it a caller wrote, `method`, `path`, a session's `caller`, `address` and `user_agent`, is each cut to 256 bytes.

A websocket resolves its caller again on every heartbeat, and that resolution does not change the line: it names the caller the connection was admitted as.

Every route the server answers writes one:
every `/api/v1` route, the document websocket and the live socket (`/ws/live`), `/auth/*`, the GitHub proxy `/api/github/rest/`, and the 404 an `unmatched` path gets (a known path called with a method its route does not serve is one too).
The dashboard's own pages and assets write none, so a browser's page loads do not bury the callers.
`/healthz`, which a load balancer calls every few seconds, `/livez`, which a liveness or startup probe calls, and `/metrics`, which a scraper calls, write none either.
Three answers are given before any route runs and write none:
the `403` a cookie request from another origin gets, the `421` a request for another host gets on a server with local sign-in, and the `307` redirect Go's router gives an unclean path (`/api/v1//issues`) or a prefix route called without its trailing slash (`/api/github/rest`).
Match the line as `msg="dispatch: request"`: the previous-shared-token warning's message starts with the same words.
The line is one synchronous write to stderr per request, so a log pipe that blocks stalls requests.

## Routes

`GET /api/v1` (no credential) is the authoritative list:
every mounted `/api/v1` route with its `method`, `path`, `auth` (`public`, `any`, `human`, or `bearer`), and `description`, sorted by path then method.
`envoy-dispatch routes` prints the same body from the table alone, with no database or listener and never a test hook;
the docs site's HTTP API reference is generated from it.
The list below is a summary.
An unknown path under `/api`, `/v1`, `/auth`, `/ws`, or `/healthz` is a JSON 404 `{"code":"NOT_FOUND","error":"no route for GET
/v1/issues","hint":"GET /api/v1 lists every route"}`, never the dashboard shell.
Every server process publishes its dashboard build's assets into the database at boot (Running more than one process, above), so a missing local file under `/assets/` is read whole from the database under the same key (`assets/<file>`) before Dispatch answers, through a cache of at most 64 MiB the process's requests share;
it never reads `index.html` or a non-asset path from there.
A path no build published remains `404 {"error":"not found"}` and any other read failure is a 502 with no cache header.
The dashboard's pages (`index.html` and the shell served for a browser route) carry `Cache-Control: no-cache` and an `ETag` of their bytes, no `Last-Modified`, and its hashed `/assets` files `public, max-age=31536000, immutable`, so a browser keeps no page from another build.


In this list, header identity means only the test/local harness mode described above.
Production Dispatch uses cookie identity through the sign-in pool.

Each item below names a path with its method and identity, then its purpose.

- `/api/v1` (GET; none)

  List every `/api/v1` route with its auth and purpose.

- `/auth/start` (GET; none)

  Start Google sign-in at the sign-in issuer:
  the Cognito sign-in pool, or Google itself for `DISPATCH_SIGNIN_DOMAIN`, asking for offline access with `prompt=consent` and the domain as `hd`, with the sign-in's state in the signed `dispatch_oauth_state` cookie.
  `503 SIGNIN_UNCONFIGURED` on a server with no `DISPATCH_SIGNIN_*`;
  `503 SIGNIN_UNAVAILABLE`, and no cookie, while the issuer's discovery document has not been read.

- `/auth/callback` (GET; sign-in state)

  Exchange the issuer's code and sign in the person `DISPATCH_SIGNIN_GROUP` or `DISPATCH_SIGNIN_DOMAIN` admits:
  under a group, named by the email in their pool username, anyone outside the group refused with a 403 page naming them;
  under a domain, named by the verified `email` of a Google account whose `hd` is the domain, anyone else refused with a 403 page giving the reason.
  Record the person and the refresh token (and, under a domain, the account's `sub`, ending the address's sessions when another account held it), issue the cookie.
  Any process holding a key of the set completes a sign-in another process started.
  `400` "invalid or expired state" when the state cookie is missing, does not verify, has lapsed or names another state;
  `503 SIGNIN_UNAVAILABLE` while this process has not read the issuer's discovery document;
  `409 SIGNIN_SUPERSEDED` and no cookie when, under a domain, another account signed in to the address between this sign-in's record and its cookie.

- `/auth/logout` (POST; cookie or trusted header)

  End the person's Dispatch sessions, forget their refresh token, and clear the session cookie.

- `/auth/whoami` (GET; cookie or trusted header)

  Return the resolved person, `{kind: "user", login, event_head}` with `login` their lowercase email and `event_head` an event id at or below which every event is final, read when the sign-in check answered.
  The page starts its events subscription from that id.
  The sign-in check comes first: a caller who is not signed in is `401 NO_IDENTITY` and the head is never read.
  A signed-in caller is `503 EVENT_HEAD_UNAVAILABLE` while no event id is proven final yet, which ends on its own and which the page asks again for, and `503 EVENT_HEAD_READ_FAILED` when the head cannot be read, which the page retries twice and then shows as an error.

- `/auth/_dev/signin` (GET; none; `DISPATCH_DEV_SIGNIN=1` only; loopback peer, no forwarding header)

  Sign in the person `login` names, lowercased, without the sign-in issuer: `302` to the sanitized `next` (default `/`) with the session cookie.
  `400 DEV_SIGNIN_INPUT` without `login`, `403 DEV_SIGNIN_FORBIDDEN` for a non-loopback peer or a request carrying `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-IP` or `Via`.
  Each mint logs at WARN with the email and the peer.
  Not mounted otherwise.

- `/api/github/rest/...` (GET; cookie or trusted header)

  Read GitHub as the GitHub App installation that covers the repository:
  a pull request (`repos/{o}/{r}/pulls/{n}`), an issue (`repos/{o}/{r}/issues/{n}`), a commit's check runs (`repos/{o}/{r}/commits/{sha}/check-runs`) or a commit's workflow runs (`repos/{o}/{r}/actions/runs?head_sha={sha}`, the query being the full 40-hex `head_sha` and at most a numeric `per_page` and `page`, each once), the only reads the web app makes;
  the checks pill joins each check run to its workflow run for the workflow name and event, keeping the newest run of each check as GitHub's merge does and dropping the runs of an event GitHub does not evaluate for a pull request (only `push`, `pull_request`, `pull_request_review`, `pull_request_target`, `deployment` and `deployment_status` count).
  A workflow-runs answer of `200` is trimmed to `total_count` and, per run in GitHub's order, only `check_suite_id`, `event` and `name`, so a run's head commit (message, author), actor and repository never reach the browser;
  a body that does not decode is `502 GITHUB_UPSTREAM`, and any other status passes through unchanged.
  The pill needs the App's Actions: read, which the delivery reconcile already needs: without it `actions/runs` answers `403` and the pill disappears.
  Every person shares the App's credential, which reads far more of a repository than that, so any other method is `405 METHOD_NOT_ALLOWED`;
  any other path, a workflow-runs read with any other query, or a path under it (`actions/runs/{id}`, its jobs or logs) `404 GITHUB_PATH_REFUSED`;
  and a repository the App is not installed on, or no App key, `503 GITHUB_TOKEN_UNAVAILABLE`.

- `/healthz` (GET; none)

  The readiness probe: `200` while this process can serve requests, which is while Postgres answers within `store.healthProbeTimeout` (two seconds) on the health pool
  — a dedicated one-connection pool, never the shared one — and `ok` says the same.
  A database that stops answering is `503` with `db: false` inside that bound, never silence, and the reason is logged.
  It is not a liveness check (`/livez` is):
  it fails while the database is unreachable, so a liveness probe on it would restart every server process during a database failover.
  Every other field is reported and never decides `ok` or the status, and a deploy check that needs one asserts it itself (`nats: true`, `files: true`, `signin: true`):
  `nats`, whether the server's publisher is connected to NATS (`null` with `DISPATCH_NATS_DISABLED=1`), so a bus outage is `200` with `nats: false` while the process serves and its outbox waits for the bus;
  the client pings every second and declares a connection lost at two pings outstanding, so a silently cut bus reads `false` within two to three seconds;
  `files`, whether the file store's bucket answers a two-second `HeadBucket` where `DISPATCH_FILE_STORE_BUCKET` is set (`null` when it is not), so a bucket outage fails uploads and downloads and nothing else;
  `signin` and `service_tokens`, whether the sign-in issuer's and the service-account token issuer's discovery documents have been read (`false` before, `null` where that issuer is not configured).
  Two seconds fits the tightest prober here, the three-second compose healthcheck and deploy script, as well as the ALB's five.
  The body also names what is deployed: `commit`, the legion commit this process's image build stamped (the Dockerfile's `LEGION_COMMIT`;
  `null` in an unstamped build); `fleet_commit`, the commit of the latest announcement that every live server process serves one build, the one `server.started` carries (`null` when there is none, or before this process's announcer has first read the table, and up to one announcer pass, five seconds, behind it);
  and `schema_version`, the highest migration `schema_migrations` records, read by the same probe (`null` when `db` is false).

- `/livez` (GET; none)

  Answer `204` with no body, reading nothing: it says only that the process answers.
  A liveness or startup probe asks it rather than `/healthz`, so a slow or unreachable Postgres, NATS or file store neither restarts the process nor adds load on every probe period.

- `/metrics` (GET; none)

  The process's metrics in the Prometheus text format:
  `envoy_dispatch_outbox_publish_failures_total{reason}`, one count per destination of an outbox publish attempt that did not reach NATS, `reason` being `denied` (the server refused it under the connection's own publish permissions), `refused` (NATS refuses the envelope or its subject however often it is retried) or `failed` (every other failure).
  It is answered above the router, so it is never the dashboard's shell, and whoever reaches the HTTP port can read it.
  Nothing scrapes it yet.

- `/api/v1/artifacts/_test/quiesce` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Close every live document and wait for the settlements in flight; not mounted otherwise.

- `/api/v1/artifacts/{id}/_test/outside-schema` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Write the crafted malformed tree e2e uses; not mounted otherwise.

- `/api/v1/live/_test/disconnect` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Close every open live socket with 1012, as a restart does; not mounted otherwise.

- `/api/v1/live/_test/rotate` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  Close every open live socket with 4001 (`max age`); not mounted otherwise.

- `/api/v1/agents/{session_id}/stream/_test/publish` (POST; cookie or trusted header (human only); `DISPATCH_TEST_HOOKS=1`)

  Feed the in-memory agent relay; `as=drop` ends agent views as `dropped`, so the page subscribes again and rebuilds from the replay; not mounted otherwise.

- `/api/v1/writes/_test/slow` (POST; cookie, trusted header, or bearer; `DISPATCH_TEST_HOOKS=1`)

  `{method, path, delay_ms}` arms or replaces a delay for that method and exact request path, and `delay_ms: 0` removes it:
  every keyed run of it waits `delay_ms` on its handler's context once its key is claimed, and a context that ends during the wait answers `503 SLOW_WRITE_CUT_OFF` with the handler never run; a bad body is `400`.
  Not mounted otherwise.

- `/ws/live` (GET; cookie, trusted header, or bearer)

  One WebSocket per browser tab, serving delivery, events, and agent subscriptions.
  An events subscription resumes after a decimal event id;
  its `events_start` says `replay_complete: true` when that id is at or below the log's head, so every event after it is replayed before it goes live and a client may skip the refresh it would owe the gap, and `false` with no resume or one above the head (a log that was reset).
  A socket rotates with 4001 at a random age from 30 to 60 minutes;
  it closes with 1012 on restart, 1008 when its caller stops resolving, and 1013 when the hub queue fills.

- `/api/v1/inbox?project=&assignee=` (GET; cookie or trusted header (human only))

  List open asks newest-first, including their issue key, title, and assignee.
  `assignee=me|unassigned|<email>` keeps asks on issues held by the caller, by nobody (project-document asks included), or by that person;
  an email nobody has signed in with is `400 ASSIGNEE_NOT_ALLOWED`.

- `/api/v1/me/answers?limit=&offset=` (GET; cookie or trusted header (human only))

  List the caller's answers and replies on asks, newest first.
  A row is either an answer with `current`, or a reply, and carries the ask's question, kind, state, owner and deep link.
  Answers come from user-authored `ask.answered` events whose actor is the answer's user; a restored answered block adds no row.
  `limit` is 1–200 (default 50), `offset` is 0 or more, and an invalid value is `400 INVALID_QUERY`.

- `/api/v1/me/replies?limit=&offset=&issue=|artifact=` (GET; cookie or trusted header (human only))

  List the threads the caller wrote in where someone else has written since the caller last read them, newest reply first, less an open ask the Inbox lists.
  A row names the thread, its owner, its subject, its root's deep link and the newest reply's author, first line and time.
  `issue` or `artifact` narrows to one owner (both is `400 INVALID_QUERY`); `limit` is 1–200 (default 50).

- `/api/v1/me/threads/{id}/read` (PUT; cookie or trusted header (human only))

  Move the caller's read mark on one thread forward to `{read_through}`;
  it never moves back, a value more than a minute ahead of the server is `400 INVALID_STATE`, and a non-uuid id is `400 THREAD_ID_INPUT`.
  Answers `204`, writing nothing for a thread the caller never wrote in.

- `/api/v1/agents` (GET; cookie, trusted header, or bearer)

  List live Envoy sessions (`session_id`, `title`, `dir`, `machine_id`, `roles`, `capabilities`, `last_seen`), newest first, from Dispatch's copy of the listener's session list, at most 5 s old;
  `503 ENVOY_UNAVAILABLE` when the listener cannot be reached, and for up to 5 s after a failed listener read without asking the listener again.

- `/api/v1/agents/{session_id}/answers?since=&max_chars=` (GET; cookie, trusted header, or bearer)

  What people wrote that Dispatch delivered to one session, newest first:
  each ask's current answer (`replaced` when it changed an earlier one), each comment at its current body (`edited_at` when edited) and each message, once however it arrived:
  a person's `ask.answered`, `comment.created`, `message.created` or `message.answered` event whose `published_destinations` hold the session's own topic, its sent message deliveries, and its sent comment deliveries (mentions).
  Never an agent's or the system's words, an ask's resolution or hand-back, or a delivery to a `role:` route.
  Each item carries its `relation`, the `ask` and its question where it answers or replies in one, and its `owner`, `ref` and `href`.
  `since` defaults to 72 hours ago (older than 7 days or in the future is `400 INVALID_QUERY`);
  `max_chars` (1–100,000) stops the list after the item whose text takes the running sum past it, `total` still counting every item;
  a bad `session_id` is `400 INVALID_SESSION_ID`.
  Not an authorization boundary: direct-message text already reaches every caller through `GET /api/v1/events`, and a person may read any session's list.

- `/api/v1/projects` (GET; cookie, trusted header, or bearer)

  List projects (`key`, `name`, `open_asks`, `created_at`), ordered by key.

- `/api/v1/projects` (POST; cookie, trusted header, or bearer)

  Create a project from `key` and `name`, for a person or an agent session alike;
  rejects a key that is not a project key, an empty name, a name holding a control character or a bidi embedding, override or isolate (U+202A-U+202E, U+2066-U+2069), and a name with no visible character (a letter, number, punctuation or symbol other than the blank fillers U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned, such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF);
  combining marks, format characters, private-use characters and whitespace don't count) with `400 INVALID_PROJECT`, a name over 80 characters (`contracts.ProjectNameMax`, UTF-16 units after the trim) with `400 CAP_EXCEEDED`, and a duplicate key with `409 PROJECT_EXISTS`.

- `/api/v1/settings/repo-projects` (GET; cookie, trusted header, or bearer)

  List repository-to-project mappings, each with `default`, whether it is its project's default repository.

- `/api/v1/settings/repo-projects/{owner}/{repo}` (PUT, DELETE; cookie, trusted header, or bearer)

  Create or replace, or remove, a repository mapping; PUT takes `{project, default?}`, and `default: true` moves the project's one default to it.
  A mapping an issue names as its `repo` is neither deleted nor moved: `409 REPO_PROJECT_IN_USE`.
  `AGENTS.md` gives the rules and the `settings.repo_project.updated` events.

- `/api/v1/github/app-installation/{owner}/{repo}` (GET; cookie, trusted header, or bearer)

  Whether Dispatch's GitHub App, which GitHub sends the repository's webhook events through, is installed on the repository:
  `200 {installed}`, `false` when GitHub answers 404.
  `503 GITHUB_APP_UNCONFIGURED` when Dispatch holds no App key, `502 GITHUB_UNREADABLE` naming the repository and GitHub's error on any other failure.
  The Legion daemon asks it for every repository it serves, for `legion state`.

- `/api/v1/settings/project-default-assignees` (GET; cookie or trusted header (human only))

  List every project that names a default assignee, by project key, as `[{project, assignee}]`; a project with no default is left out.
  Human-only, since the default names a person.

- `/api/v1/projects/{key}/default-assignee` (PUT; cookie or trusted header (human only))

  Set a project's default assignee `{assignee}` (the email of a person who has signed in, or `null` to clear it):
  who an issue created with no assignee, by nobody, under no parent or under a parent with nobody on it, goes to — the last step of the create chain.
  `400 ASSIGNEE_NOT_ALLOWED` for an email nobody has signed in with, `400 INVALID_PROJECT` for a body without `assignee` or one whose `assignee` is neither a string nor null, `404 NOT_FOUND` for an unknown project.
  Human-only: no agent surface sets it.

- `/api/v1/delivery/session-groups` (POST; cookie, trusted header, or bearer)

  Create a Delivery session group `{name, sessions}`: its sessions' pull requests take it as their Parent agent, `group:<id>`.
  `409 SESSION_IN_GROUP` for a session another group holds, `409 SESSION_GROUP_NAME_TAKEN` for a name another group has (ignoring case), `409 SESSION_GROUP_NAME_IS_SESSION_TITLE` for a name that is the latest title of a session outside the group, `400 SESSION_GROUP_INPUT` for a blank name or no sessions.
  The timeline lists every group (`session_groups`).

- `/api/v1/delivery/session-groups/{id}` (PATCH, DELETE; cookie, trusted header, or bearer)

  Rename a session group, add sessions to it or remove them `{name?, add?, remove?}`, with the create route's refusals and `400 SESSION_GROUP_INPUT` for an edit that would leave it empty;
  or delete it, its sessions showing as themselves again.
  `400 SESSION_GROUP_ID_INPUT` for an id that is not a UUID, `404 NOT_FOUND` for one no group has.

- `/api/v1/me/agent-tokens` (GET, POST; cookie or trusted header (human only))

  List personal-token metadata or mint a personal agent token.

- `/api/v1/me/agent-tokens/{id}` (DELETE; cookie or trusted header (human only))

  Revoke a personal agent token.

- `/api/v1/users` (GET; cookie or trusted header (human only))

  Everyone who has signed in, as `{users: [{login}]}` sorted by email — the assignee picker's options.

- `/api/v1/whoami` (GET; cookie, trusted header, bearer, or a broker-issued DPoP token)

  Who the server takes the caller for: `{kind: "user", login}` for a person (`login` their lowercase email), `{kind: "agent", owner, service, session, project}` for every other credential (`owner` is a personal token's owner or a broker-issued token's person, by lowercase email, null for the shared token;
  `service` is a verified service-account token's Kubernetes subject, null for every other credential;
  `session` is the session the credential itself proved, which only a broker-issued token does, null otherwise;
  `project` is the one project key that credential may write, null where it names none).

- `/api/v1/dashboard/builds` (GET; cookie, trusted header, or bearer)

  Which dashboard builds are served: `{serving, live}`, `serving` the entry chunk (`/assets/index-*.js`) of the build this process serves (`""` when it serves none), `live` the distinct entry chunks of the builds the live server processes serve (the registry of live server processes joined to `dashboard_builds` on the build id), sorted.
  The dashboard offers "A new version is available" only when `live` holds exactly one entry and it is not the tab's own, so no tab is told to reload while a rolling update or a rollback still runs two builds.
  An older process answers `404`, which the dashboard reads as no new version.

- `/api/v1/issues?project=&status=&parent=&priority=&updated_since=&route_status=&limit=&offset=` (GET; cookie, trusted header, or bearer)

  List issue summaries: every matching issue as an array, or, with `limit` (1–250) or `offset` (0 or more;
  alone it pages 50), one page `{issues, total, limit, offset}` cut after every filter, `total` counting the issues they match.
  A repeated, blank, non-integer or out-of-range `limit` or `offset`, or any `cursor`, is `400 INVALID_QUERY` naming the parameter.
  The order is status, rank, creation time and key, so consecutive offsets cover the listing once while it does not change between reads;
  an issue that enters or leaves what the filters match, or whose status or rank changes, between two reads shifts rows across a page boundary, so one issue is served twice and another never.
  Only the unpaged array is an exact set in one read.
  Filters are optional; `updated_since` is RFC3339 and inclusive, matching issue changes and later issue events.
  `priority` repeats (`priority=0&priority=1`), each value `0`–`3` or `none` for an issue with no priority; any other value is `400 INVALID_PRIORITY`.
  `route_status` (`live`, `no_holder` or `unknown`;
  anything else is `400 INVALID_ROUTE_STATUS`) keeps the open issues whose route is in that state, whatever their priority;
  `live` or `no_holder` is `503 ENVOY_UNAVAILABLE` when the listener does not answer.
  Summaries contain `key`, `title`, `status`, `priority`, `parent`, `assignee`, `route`, `route_status`, `route_holder`, `updated_at`, `last_seq`, and `open_asks`.
  Every issue read (this list, `?pinned=true`, and `GET /api/v1/issues/{key}`) resolves `route_status` against the process's one copy of the listener's `GET /v1/sessions`, stored in no table:
  that copy is at most 5 s old, one listener call refreshes it for every read that asks while it runs, and a listener that fails or does not answer within 2 s is asked again only after 5 s, every read in that window reading `unknown` at once.
  The status is `live` (a live session holds the role, or the session is live;
  `route_holder` names it), `no_holder` (nobody live holds the role, or the session is not live), `unknown` (the listener did not answer), or null with no route.

- `/api/v1/search?q=&project=&limit=&offset=` (GET; cookie, trusted header, or bearer)

  Full-text search over issue titles, latest document text, comments, asks, and messages;
  each kind ranked on its own and merged by reciprocal rank fusion (above), with `<mark>` snippets and SPA `href`s.
  One page `{results, total, reachable, limit, offset, took_ms}`:
  `limit` 1–50 (default 20), `offset` 0 or more (default 0), `total` every match, `reachable` the hits the pages can return (each kind's best 100).
  Every `ask` hit carries `ask`, where the ask stands when the search ran:
  `{state: "open"}`, `{state: "answered", answer}` with its latest answer, or `{state: "resolved", resolution, answer?}` with the last answer it had, absent when nobody answered;
  its excerpt is cut from the ask's indexed text ([Search](#search)).
  An under-two-character query returns `400 INVALID_QUERY`;
  a stop-word-only query returns `200` with no results;
  `400 CAP_EXCEEDED` over 1,000 characters (`contracts.SearchQueryMax`, UTF-16 units after trimming), since the query rides in the URL;
  `400 INVALID_PROJECT` for a project that is not a project key (none searches every project); `400 INVALID_LIMIT`; `400 INVALID_OFFSET`.
  Keyword search finds a text whose search vector would pass Postgres's limit on one vector by the words in its opening part only ([Search](#search)).

- `/api/v1/search/coverage` (GET; cookie, trusted header, or bearer)

  Meaning search's coverage, counted in one statement ([Search](#search)):
  `{identity, place, model, total, kinds}`, the configured embedder's identity, its place (`bedrock` or `azure`) and its model, all three null with no embedder;
  then, in `total` and for each kind in `kinds` (`issue`, `document`, `comment`, `ask`, `message`), `items` (the searchable ones, a text with nothing to embed left out), `current` (embedded at the item's current text by that identity), `failed` (dead and not current), `lacking` (the rest) and `oldest_lacking_seconds`, how long the longest-waiting lacking item has waited (null when none lacks).
  With no embedder nothing is current.

- `/api/v1/projects/{key}/related?title=&text=&exclude_issue=&keywords_only=` (GET, POST; cookie, trusted header, or bearer)

  Run the related-item check an issue create runs ([Search](#search)) on a title and a text, checked as the title, a newline and the text (either alone is enough), and write nothing:
  no issue, ask, suggestion or `related_checks` row.
  `exclude_issue` leaves that issue of the project, and everything it holds, out of the results and out of the word statistics (`400 EXCLUDE_ISSUE_INPUT` when it is not an issue of the project);
  `keywords_only=true` skips meaning search.
  GET takes the fields in the query, title and text together at most 1,000 UTF-16 units (`400 CAP_EXCEEDED` past it);
  POST takes them as a JSON body `{title, text, exclude_issue, keywords_only}`, a title of at most 1,000 and a text as long as the body's 1 MiB bound.
  It waits at most 5 seconds and answers `200` with the create's `advice.suggestions` shape (`outcome`, `search`, `search_note`, `search_query`, `related`, `decision`, `missing`), a blank title and text included (`not_made`).
  `400 INVALID_PROJECT` for a key that is not a project key, `404 PROJECT_NOT_FOUND` for an unknown one.

- `/api/v1/related-checks?project=&source_kind=&from=&to=` (GET; cookie, trusted header, or bearer)

  Count the related-item checks recorded when an issue, an ask or a new decision block was written, within `[from, to)` (RFC3339, each optional) and of `project` and `source_kind` (`issue`, `ask` or `decision_block`;
  another is `400 INVALID_QUERY`) when given:
  `{project, source_kind, from, to}` echo the filters (null when absent), then `total`, `by_source_kind`, `by_outcome`, `by_search` (a check not made counted under `not_made`), `by_reason`, `completed` (found or none), `skipped_for_time` (reason `timeout`), and `keyword_ms` and `total_ms`, each `{count, p50, p95, max, over_300ms}` over the checks holding that time (`model.RelatedChecksReport`).

- `/api/v1/issues/{key}/references?versions=` (GET; cookie, trusted header, or bearer)

  Read the issue's eight-hop artifact reference closure; a reference to a block reaches the block's document while a current version holds the block.
  An `If-None-Match` value equal to the response ETag returns `304`.
  `versions=latest` leaves each member's `artifact.versions` holding only its latest version and carries its own ETag;
  absent, empty or `all` is the full answer; any other value, or `versions` given more than once, is `400 INVALID_QUERY`.

- `/api/v1/references?to=|from=&kind=&since=` (GET; cookie, trusted header, or bearer)

  Edges of one node in the reference graph, newest first and cross-project:
  exactly one of `to` (backlinks) or `from` (links), each a `dispatch://` reference, read whatever its length (`text.ParseRef`);
  `kind` filters a csv of edge kinds; `since=<events.id>` keeps mentions introduced after it (structural edges excluded).
  The answer is `{node, edges}`, the node read and its edges.
  Each edge carries the other `node`, an `excerpt` (the containing block for a document mention), `created_at`, and `source_seq`.
  The node read, each edge's `node` and each edge's `block` carry the six facts of every node, `made_by`, `made_at`, `changed_by`, `changed_at`, `parent` and `parent_version` ([Reference graph](#reference-graph)).
  A mention a block of a document holds is listed from that block (`node.kind` `block`, `node.document` its document), and one a block of a comment or issue message holds from that block (`node.kind` `block`, id `comment/<uuid>#<block id>` or `message/<uuid>#<block id>`, `node.parent` the comment or message);
  a document's, comment's or message's own read also lists the edges at its blocks, each naming the block as `block`, and leaves out their `part_of` edges unless `kind` names `part_of`, and a reply that names a block of the comment or message read is listed once, under that block.
  A block reference (`#b-<id>`) names a `block` node carrying its whole current `text`;
  pinned to a version (`#b-<id>@v<N>`; for a comment's block, a revision of the comment) it also carries `pin` (`version`, `ref`, the block's `text` at that version, `state` `unchanged`, `changed` or `removed`, and `removed_version`), and a removed block answers no edges.
  `400 INVALID_REFERENCE` / `INVALID_KIND` / `INVALID_SINCE`, `INVALID_REFERENCE` too for a message block pinned with `@v<N>` (a message never changes);
  `404` for a node that does not exist, a block no current version or revision holds (unpinned) or whose id is past 256 bytes (no node), or a pinned version the document lacks, has no record of the blocks of, or did not hold the block at, or a revision past the comment's current one or that did not hold the block.
  A `since` read stops at the event log's safe horizon, and first waits up to 1 s for the horizon to pass every event committed before the read, so it answers every mention committed before it, the caller's own included;
  when an open transaction holds the horizon back past that wait, it answers the mentions at or below the horizon and a later read the rest.

- `/api/v1/block-links?from=&to=` (GET; cookie, trusted header, or bearer)

  The links from blocks of the document `from` names to blocks of the document `to` names (an issue's spec or document, or a project's document;
  one document may be both), each reference read whatever its length:
  `{from, to, links}`, each link with its `kind`, both block nodes, and the `created_at` and `created_version` at which the source block first held it, newest first;
  `from`, `to` and every link's two blocks carry the six facts of every node ([Reference graph](#reference-graph)).
  A link is listed only while both blocks are current blocks of their documents.
  `400 INVALID_REFERENCE` when either is not a document reference; `404 NOT_FOUND` for a document that does not exist.

- `/api/v1/issues` (POST; cookie, trusted header, or bearer)

  Create an issue and its primary document.
  Omitting or leaving `spec` blank gives an empty primary document at version 1.
  `repo` (`owner/name`) is the issue's own repository and must be mapped to its project:
  `400 INVALID_ISSUE` when it is not `owner/name`, `400 REPO_NOT_MAPPED` when it is not mapped.
  A title over 1,000 characters (`contracts.IssueTitleMax`, UTF-16 units after trimming) is `400 CAP_EXCEEDED` before the duplicate check.
  Refuses a title that near-duplicates an issue in the project with `409 POSSIBLE_DUPLICATE` and candidates unless `force` is true; external references skip the check.
  A spec whose ask block breaks its content rule (`paragraph+ bullet_list?`:
  one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`, and a spec whose markdown makes more than 65,536 elements (weighed as Element weights below lists) is `413 CAP_EXCEEDED`, naming the line that passes the limit;
  so is one whose markdown as stored (what `.../text` answers) would be longer than 1 MiB or make more than 65,536 elements.

- `/api/v1/issues/{key}` (PATCH; cookie, trusted header, or bearer)

  Update an issue's `title`, `status`, `priority`, `labels` (or `labels_add`, which adds to the labels the issue holds when the write commits;
  not with `labels`), `route`, `external_links`, `parent`, `rank`, `assignee`, `components` or `repo` (`null` clears it); `AGENTS.md` gives each field's rules.
  A title is trimmed, and one over 1,000 characters is `400 CAP_EXCEEDED`, as on creation.
  `expected_sequence` makes the write conditional: `409 ISSUE_CHANGED` when the issue's last event is no longer that sequence.
  A close (`status: done`) withdraws every open ask of the issue, whoever's turn it is:
  decision blocks in its documents, plain asks and requests to approve one of its documents.
  Each is recorded `resolved` with resolution kind `withdrawn`, naming the closer as the request's actor, with the reason `Withdrawn by the close of <KEY>.`, and gets one `ask.resolved` before `issue.closed`;
  its block is written `state="resolved"` in the live document, and no version is written.
  An ask answered or resolved before the close is left as it was, and the close records no answer and no approval.
  A reopening `status` (any but `done`) opens again exactly the asks the close withdrew, each with one `ask.opened` before `issue.updated`, and they return to the Inbox.

- `/api/v1/issues/{key}/summary` (GET; cookie, trusted header, or bearer)

  What a reference to an issue shows, read without the full issue:
  `{key, project, title, status, documents}`, `documents` every artifact of the issue in the order the issue read lists them, each `{id, slug, name, kind, latest_version, primary}`, `primary` marking the spec, which a reference to a block of the spec names.
  One query for the issue and one for its artifacts, with no version history, approval, child or open-ask read.
  `404 NOT_FOUND` for no such issue.

- `/api/v1/issues/{key}/asks` (POST; cookie, trusted header, or bearer)

  Create an optionally anchored ask.
  An anchor is exactly `{artifact, quote, occurrence?}` for a server-written quote mark or `{artifact, mark_id}` for a mark already written by a browser.
  A quote mark carries the ask's id and who asked, which no rendering shows and every load of the document builds:
  the marks a document's comments, suggestions and asks hold on its text carry at most 1 MiB of ids and authors, and a quote anchor that would leave them past it and carrying more is `413 CAP_EXCEEDED`, naming the bound and both sizes.

- `/api/v1/issues/{key}/asks?state=&thread=` (GET; cookie, trusted header, or bearer)

  List an issue's asks, open and/or answered (`state`: `all` default, `open`, or `answered`).
  Each row carries `last_reply`, the newest comment in the ask's thread or null.
  `thread=1` adds `thread`, its `replies`, `edits`, `answers` and `followers`, batched over the list so no card fetches its own;
  without it no `thread` is returned, which is what the readers that render the ask alone want from this unpaged list.
  Any other `thread` value is `400 INVALID_ASK_THREAD`.

- `/api/v1/asks/{id}` (GET; cookie, trusted header, or bearer)

  Read an ask with its reply thread, edits, followers and `answers`, every user-authored answer oldest first.

- `/api/v1/asks/{id}` (PATCH; cookie, trusted header, or bearer)

  Edit one or more of `question`, `options`, `multiple`, or `urgency` on an open ask.
  A bearer caller must be the asking session; a human may edit any open ask.
  The response records `edited_at` and emits `ask.edited` with the current ask, prior mutable fields, and `edited_by`.
  Anchors are selected when the ask is created and cannot be changed by this route.
  On a block ask the `:::ask` block is written in the same transaction and the row takes the block's parsed values, so the edit versions the document once and no settlement reverts it.
  Only the named fields are written: `urgency`/`multiple` alone go through the attribute path and leave the body's nodes, marks and inner block ids untouched, so an untouched question keeps its formatting, links and comment anchors;
  naming `question` or `options` replaces that part with the markdown pipeline's own parse, and anchors inside the replaced text move as for any document edit.
  A field named but unchanged is not rewritten, so an idempotent retry of the whole ask writes nothing, versions nothing, keeps every anchor and returns 200.
  Text the block cannot carry back unchanged
  — an option label containing `": "`, the separator between a label and its description, is one example
  — is `400 ASK_BLOCK_TEXT` naming the field, with nothing written.
  Text past what one write's markdown may make - weighed before it is rendered, an element for each backslash, `*`, `_`, `~`, backtick, `[`, `]` and `<` and two for each line feed, then as its markdown parses - is `413 CAP_EXCEEDED`.
  An edit of a block ask that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, is `413 CAP_EXCEEDED` and the ask keeps its text.

- `/api/v1/asks/{id}/answer` (POST; cookie or trusted header)

  Answer an open ask, or change the current answer with `expected_answer_at` set to that answer's `at`.
  Without it an answered ask is `409 ASK_CLOSED`.
  A stale value, or one sent for an open ask, is `409 ASK_ANSWER_CHANGED`; a different person is `403 NOT_ANSWERER`; an approval is `409 ASK_APPROVAL_REVIEW`.
  A change appends another `ask.answered` with `previous_answer`.
  An approval ask is moved to each document version before the human sees it, and an `ASK_EDITED` response means the question changed after the human reviewed it, so they reload and answer the moved request.
  On a block ask the answer is written into its block, and a changed answer without text removes the block's old `answer`.
  Its words and the options it chooses, whose labels are the asker's text, are weighed:
  where they would leave the document's markdown longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, the ask still takes the whole answer and the block is written without them, saying who answered and when.
  The answer's state and who gave it and when are not weighed, so the asks of a document already past those limits can still be answered.
  The answer is kept on the ask as well: a block that leaves the document and returns gets it back at settlement, which weighs each returning answer on its own in document order and keeps one on the ask alone, the block saying who answered and when, while writing it back would take the document past those limits and make it bigger.

- `/api/v1/asks/{id}/resolve` (POST; cookie, trusted header, or bearer)

  Retract or self-resolve an open ask with a recorded reason.
  On a block ask the block's `state` is written with it, and that is not weighed, so an ask on a document past what one upload may hold can still be resolved.
  A reason beginning `removed from the document in version`, which marks a retraction settlement wrote and would have the retract undone when the block returns, is `400 INVALID_RESOLUTION`.
  The kind is `retracted` or `resolved`, and any other is `400 INVALID_RESOLUTION`: `withdrawn` is written only by an issue's close.
  An ask on a closed issue is `409 ISSUE_CLOSED`; reopen the issue first.

- `/api/v1/issues/{key}/comments?artifact=` (GET; cookie, trusted header, or bearer)

  List comments, optionally limited to an artifact ID.

- `/api/v1/issues/{key}/comments` (POST; cookie, trusted header, or bearer)

  Create a comment, root-level reply, or suggestion.
  An anchored comment uses the same quote-or-mark-ID shape as an ask; replies inherit their root's anchor and send none.
  A reply may name the block of the comment `reply_to` names that it answers, `reply_to_block` (`b2`), checked against that comment before `reply_to` climbs to its thread's head:
  `400 INVALID_COMMENT` without `reply_to`, for a block that comment does not hold now, and in an ask's thread.
  The delivery callback `POST /api/v1/comments/{id}/reply` takes `reply_to_block` for a block of comment `{id}` the same way.
  A comment's record in its document's margin - its body and replies, or a suggestion's and its replacement - holds at most 256 KiB of text, and a document's margin 1 MiB:
  a comment, reply or suggestion that would leave either past its bound and bigger is `413 CAP_EXCEEDED`, naming the bound and both sizes.
  A quote anchor that would leave the marks on the document's text carrying more than 1 MiB of ids and authors, and more than they carried, is `413 CAP_EXCEEDED` too, as for an ask's.

- `/api/v1/comments/{id}` (GET; cookie, trusted header, or bearer)

  Read a comment and its reply chain.
  Every comment a route or event returns carries `reply_block` (`{node, ref, block_id, text, removed}`), the block of the comment it answers, `null` when it names none:
  `text` the block's text cut to 480 characters, its last text once removed, and `removed` true while the comment's current revision no longer holds it.

- `/api/v1/comments/{id}/blocks` (GET; cookie, trusted header, or bearer)

  A comment's current blocks in body order:
  `{item, revision, indexed, blocks: [{id, type, text, ref, from, to, changed_revision, changed_at}]}`, `item` the comment's graph node, `revision` its indexed revision, `from` and `to` UTF-16 code units into its stored body.
  While the comment is owed a block index, or was indexed from another body than it stores, `indexed` is `false` and `blocks` empty; the read writes nothing.
  `400 COMMENT_ID_INPUT` for a non-uuid id.

- `/api/v1/comments/{id}` (PATCH; cookie or trusted header)

  Edit a comment body.
  Human authors only.
  An edit that would leave the comment's margin record or its document's margin past its bound (above) and bigger is `413 CAP_EXCEEDED`.
  `blocks` (`[{id, from}]`, each block id the editor carried and where its span starts in `body`, in UTF-16 code units, in body order) with `base_revision` (the revision `.../blocks` answered) keeps each id on the new block its span starts in;
  with a `base_revision` that is no longer the comment's, or without `blocks`, the new blocks take ids by their text.
  `400 INVALID_COMMENT` naming the field for `blocks` without `base_revision`, and, at the current revision, for an id that is not a current block of the comment, an id twice, or a `from` outside `body` or before the one ahead of it.

- `/api/v1/comments/{id}/resolve` (POST; cookie, trusted header, or bearer)

  Resolve a comment.

- `/api/v1/comments/{id}/reopen` (POST; cookie or trusted header)

  Reopen a resolved thread-root comment.

- `/api/v1/comments/{id}/accept` (POST; cookie or trusted header)

  Apply and accept an anchored suggestion.
  An accept that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, is `413 CAP_EXCEEDED` and leaves the suggestion open.
  A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and leaves the suggestion open;
  one removed after it answers `200` with `lost: true`.

- `/api/v1/comments/{id}/reject` (POST; cookie or trusted header)

  Reject a suggestion.

- `/api/v1/issues/{key}/messages` (POST; cookie, trusted header, or bearer)

  Post an issue message.
  A reply may name the block of the message `in_reply_to` names that it answers, `in_reply_to_block` (`b2`), as may `POST /api/v1/agents/{session_id}/messages` and, for a block of message `{id}`, `POST /api/v1/messages/{id}/reply` (not on a follow-up):
  `400 INVALID_MESSAGE` without `in_reply_to` and for a block that message does not hold now.
  A reply naming a block carries that block's text as `reply_body`.

- `/api/v1/issues/{key}/messages` (GET; cookie, trusted header, or bearer)

  List an issue's message threads: every root message, oldest first, unpaged, each with its delivery attempts and its replies.
  Every message a route or event returns carries `reply_block`, the block of the message it answers, as a comment does.

- `/api/v1/messages/{id}/blocks` (GET; cookie, trusted header, or bearer)

  A message's current blocks in body order:
  `{item, indexed, blocks: [{id, type, text, ref, from, to}]}`, `item` the message's graph node, `from` and `to` UTF-16 code units into its stored body, `ref` absent for a direct message's block, which has no address.
  Read under `GET /api/v1/messages/{id}`'s rule:
  a bearer reading a direct message names its session in `?session=` (`400 SESSION_REQUIRED`, `403 THREAD_FORBIDDEN`).
  While the message is owed a block index, `indexed` is `false` and `blocks` empty; the read writes nothing.

- `/api/v1/issues/{key}/artifacts` (GET, POST; cookie, trusted header, or bearer)

  List issue artifacts or create a version from a multipart `file` or JSON `{name, content, summary?, actor?}`.
  The JSON form requires `Content-Type: application/json`.
  A `text/markdown` file is a document.
  Any other file's kind is read from its bytes, never from the part's declared `Content-Type`:
  it is `image` only when its bytes are a PNG, JPEG, GIF or WebP, stored with `mime` the type its bytes are (a JPEG declared `image/png` is `image/jpeg`), and otherwise a `file` under its declared type, an SVG among them, so no client stores other bytes as an image.
  A version whose kind is not its artifact's is `400 ARTIFACT_KIND_MISMATCH`.
  A name no artifact has yet is `400 INVALID_ARTIFACT`, naming the bound, when it, or the reference key `<owner>/<slug>` it gives the new artifact, is over 2692 bytes, the most a B-tree entry of `artifacts_ref_key_key` holds of incompressible text;
  an upload under an existing artifact's name, one stored longer included, is never refused for its length.
  An ask block whose body breaks its content rule (`paragraph+ bullet_list?`:
  one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes.
  A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose markdown makes more than 65,536 elements (weighed as Element weights below lists), naming the line that passes the limit;
  a document or version whose markdown as stored (what `.../text` answers) would be longer than 1 MiB and longer than the document's was, or make more than 65,536 elements and more than the document's did, or that would leave a live state ygo could not decode as one update;
  or whose formatting is more items than one document update can store (1,048,576), naming the count.
  A file the configured bucket refuses is `502 FILE_STORE_UNAVAILABLE`, with no version written.
  A fresh POST to a closed issue is `409 ISSUE_CLOSED` before its body is spooled, keyed or not, and claims no key;
  a repeat under a key Dispatch answers itself (a stored answer, a first still running, a write that committed and lost its answer) is answered as that key says whatever the issue's state now, so an upload that landed before the close is replayed its `201`;
  a malformed `Idempotency-Key` is `400 KEY_INVALID` first.

- `/api/v1/projects/{key}/artifacts` (GET, POST; cookie, trusted header, or bearer)

  List non-primary project artifacts (or only unlinked documents with `?unlinked=true`), or create an unlinked project artifact, its kind read, and a new artifact's name held to 2692 bytes, as an issue upload's are.
  An ask block whose body breaks its content rule (`paragraph+ bullet_list?`:
  one or more paragraphs, then at most one bullet list, last) is `400 INVALID_ASK_BLOCK`; a new version is held to it only for the asks it writes or changes.
  A markdown document over 1 MiB, or any file over 25 MiB, is `413 CAP_EXCEEDED`, and so is a document whose markdown makes more than 65,536 elements (weighed as Element weights below lists), naming the line that passes the limit;
  a document or version whose markdown as stored (what `.../text` answers) would be longer than 1 MiB and longer than the document's was, or make more than 65,536 elements and more than the document's did, or that would leave a live state ygo could not decode as one update;
  or whose formatting is more items than one document update can store (1,048,576), naming the count.
  A file the configured bucket refuses is `502 FILE_STORE_UNAVAILABLE`, with no version written.

- `/api/v1/artifacts/{id}` (GET; cookie, trusted header, or bearer)

  Read an artifact, its versions, and incoming references.
  `{id}` must be a UUID.
  Every version on every artifact read and list (this route, the issue and project artifact lists, the issue read, the reference reads, and each version write's answer and event) carries `size`:
  the bytes it stores, a file's or an image's bytes or a document's markdown, read from the row's own length without rendering the document or loading its history.

- `/api/v1/artifacts/{id}/rebuild` (POST; cookie or trusted header (human only))

  Rebuild a document only when its durable history cannot load (`409 DOCUMENT_UNLOADABLE` on its reads):
  deletes its document updates, checkpoints, and snapshots, then writes one fresh update from the latest saved version or optional `{markdown}`.
  A supplied markdown source that changes the document writes the next immutable version, emits `artifact.version`, and moves an open approval request to that version, where it waits on its agent, as every version does;
  an omitted source keeps the existing latest-version behavior.
  The rebuild, that version, its event and the move commit in one transaction, and every refusal comes before anything is written:
  a live room is `409 DOCUMENT_LIVE`; a healthy, non-resident document is `409 DOCUMENT_LOADS`;
  supplied markdown on a closed issue's document is `409 ISSUE_CLOSED`;
  supplied markdown held to a new document's limits - more than 65,536 elements, or stored as markdown longer than 1 MiB or making more than 65,536 elements - is `413 CAP_EXCEEDED`.
  The latest saved version rebuilds whatever it weighs, except a document whose tree is more items than ygo decodes in one update (1,048,576), which no document can hold and which is `413 CAP_EXCEEDED` naming the count, from either source.
  A refused or failed rebuild leaves the document unchanged.
  It answers the report, whose `source_version` is the version the rebuilt document holds: its latest saved version, or the version supplied markdown wrote.

- `/api/v1/artifacts/{id}/text` (GET; cookie, trusted header, or bearer)

  Read a live document's markdown.
  `{id}` must be a UUID.
  A stored tree outside the Proof schema is `409 DOC_SCHEMA`, and a stored history that cannot load `409 DOCUMENT_UNLOADABLE` (Document errors).

- `/api/v1/artifacts/{id}/versions/{n}` (GET; cookie, trusted header, or bearer)

  Read a document version or download a blob.
  `{id}` must be a UUID.
  A file whose bytes are in the bucket is `502 FILE_STORE_UNAVAILABLE` when the bucket does not answer, `503` on a server with no bucket configured.
  A file or image version, whoever owns it, is served with `Cache-Control: private, max-age=31536000, immutable` beside its `ETag` (the bytes' sha256), since a version's bytes never change and the route is authenticated;
  a document version, and every error, carries no cache header.

- `/api/v1/artifacts/{id}/versions` (POST; cookie, trusted header, or bearer)

  Create a named live-document version.
  `{id}` must be a UUID.

- `/api/v1/artifacts/{id}/edits` (POST; cookie, trusted header, or bearer)

  Apply document edit operations.
  `{id}` must be a UUID.
  An edit is `400 INVALID_ASK_BLOCK` when an ask it writes or changes breaks its content rule (`paragraph+ bullet_list?`) or holds what settlement cannot read;
  an ask it carries through unchanged is not its to refuse.
  A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing;
  one removed after it answers `200` with `lost_ops`.
  A batch whose markdown (its inserts and replaces together) makes more than 65,536 elements, or that would leave the document's markdown (what `.../text` answers) longer than 1 MiB and longer than it was, or making more than 65,536 elements and more than it did, or a live state ygo could not decode as one update, is `413 CAP_EXCEEDED` with the refusal itself as the error, and writes nothing.

- `/api/v1/artifacts/{id}/approval-requests` (POST; cookie, trusted header, or bearer)

  Ask a human to approve a document's latest settled version.
  The question is `Approve <name> (version <N>)?` followed by the optional `summary` (blank is `400 SUMMARY_INPUT`;
  past the ask cap at the longest version a request can reach, ten digits, is `400 CAP_EXCEEDED` naming `summary`).
  A request on a document a human approved earlier names that approval in `ask.approval.approved` (`{version, at, by}`);
  a first request carries no `approved` key.
  One open approval row follows every document version in place, rewording its question and emitting `ask.edited`;
  while its `requested_version` is below the new version it waits on the agent.
  Only the first move since the request was opened or handed back notifies, even when a thread reply had already left it waiting on the agent;
  a later move, while `requested_version` is already below the version it named, is `quiet: true`, `notify: false`, and reaches no follower.
  A version a human's answer to a decision block writes, when that answer is the whole change since the version he approved (who answered what is the document's asks rows, and an ask block no row of the document backs, a copy, one settlement could not read, one whose server-owned attributes are not as settlement writes them from its row, or a block id the version repeats carries nothing), carries his approval to it in the same transaction - an `approved` review in his name whose `reason` names the carry, and `artifact.approved` - and answers the open request `Approve` in his name after the move (`ask.answered`, the review's `ask_id`), so nothing is requested again for it.
  Calling this route again while the row waits on the agent - moved, or a thread reply newer than its last hand-back holds the turn - hands it back to the human:
  a summary that changes its question rewords it first (`ask.edited`;
  an omitted summary keeps its prior one), then `requested_version` becomes the latest version and `ask.handed_back` is emitted, leaving `edited_at` as it was.
  While the row waits on the human, the same summary or none is a repeat that answers `200` with no event, and a different one is `409 APPROVAL_WAITS_ON_HUMAN` and changes nothing, since it would rewrite the card the human is reading.
  It answers `201` when it wrote anything, with the document's `approval` as the call left it (`waiting_on` while awaiting).

- `/api/v1/artifacts/{id}/asks?state=` (GET, POST; cookie, trusted header, or bearer)

  List or create asks on an unlinked document.

- `/api/v1/artifacts/{id}/comments` (GET, POST; cookie, trusted header, or bearer)

  List or create comments and suggestions on an unlinked document.

- `/api/v1/artifacts/{id}/events` (GET; cookie, trusted header, or bearer)

  Read an unlinked document's events.

- `/api/v1/artifacts/{id}/references` (GET; cookie, trusted header, or bearer)

  Read an artifact's outgoing and incoming reference edges.
  A link a block of the artifact holds is outgoing from the artifact, a block target listed with its document as `artifact` while a current version holds the block;
  every item and document citing the artifact or one of its current blocks is listed once, a document as itself.

- `/api/v1/issues/{key}/artifacts/{slug}` (GET; cookie, trusted header, or bearer)

  Read an issue artifact and its incoming references.
  `{slug}` is resolved within `{key}`.

- `/api/v1/issues/{key}/artifacts/{slug}/text` (GET; cookie, trusted header, or bearer)

  Read an issue artifact's live document markdown.

- `/api/v1/issues/{key}/artifacts/{slug}/versions/{n}` (GET; cookie, trusted header, or bearer)

  Read an issue artifact version or download its blob, as `/api/v1/artifacts/{id}/versions/{n}` does.

- `/api/v1/issues/{key}/artifacts/{slug}/versions` (POST; cookie, trusted header, or bearer)

  Create a named issue-document version.

- `/api/v1/issues/{key}/artifacts/{slug}/edits` (POST; cookie, trusted header, or bearer)

  Apply issue-document edit operations.
  A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing;
  one removed after it answers `200` with `lost_ops`.

- `/api/v1/projects/{key}/artifacts/{slug}` (GET; cookie, trusted header, or bearer)

  Read an unlinked project artifact and its incoming references.
  `{slug}` is resolved within `{key}`.

- `/api/v1/projects/{key}/artifacts/{slug}/summary` (GET; cookie, trusted header, or bearer)

  What a reference to a project document shows, read without its versions:
  `{id, project, slug, name, kind, latest_version}`, `{slug}` (a slug or unambiguous filename) resolved as the project document read resolves it (`404 ARTIFACT_NOT_FOUND`, `400 ARTIFACT_REFERENCE_AMBIGUOUS`).

- `/api/v1/projects/{key}/artifacts/{slug}/text` (GET; cookie, trusted header, or bearer)

  Read an unlinked project document's live markdown.

- `/api/v1/projects/{key}/artifacts/{slug}/versions/{n}` (GET; cookie, trusted header, or bearer)

  Read an unlinked project document version or download its blob, as `/api/v1/artifacts/{id}/versions/{n}` does.

- `/api/v1/projects/{key}/artifacts/{slug}/versions` (POST; cookie, trusted header, or bearer)

  Create a named project-document version.

- `/api/v1/projects/{key}/artifacts/{slug}/edits` (POST; cookie, trusted header, or bearer)

  Apply project-document edit operations.
  A change a concurrent browser deletion removes before the version is rendered is `409 EDIT_LOST_TO_CONCURRENT_CHANGE` and writes nothing;
  one removed after it answers `200` with `lost_ops`.

- `/api/v1/agents/{session_id}/artifacts` (POST; cookie, trusted header, or bearer)

  Upload a file or image to an agent's conversation:
  a multipart `file` (+ `name`, `summary?`, `actor?`) with an issue upload's caps, errors and file-store write, its kind read from its bytes as an issue upload's is (an `image` only for a PNG, JPEG, GIF or WebP, stored under the type its bytes are;
  any other file a `file` under its declared type), stored with `session_id` set, `issue_key` null, `project` `""` and `ref_key` `agent/<session_id>/<slug>`, addressed `dispatch://agent/<session_id>/artifact/<slug>`;
  it appends no event, since every event belongs to an issue, a document or a project.
  There is no list route: a conversation's pictures are reached through the references its messages carry, never enumerated.
  A caller whose credential proves a session of its own — an access token the secrets broker issued, the one that does — uploads only into that session's conversation;
  any other `session_id` is `403 SESSION_SCOPE`, naming the session the credential acts for and the session addressed.
  A person's cookie proves no session and uploads into the conversation it names.
  An agent's conversation holds files and images only, so a markdown document (JSON `content` or a `text/markdown` file) is `400 ARTIFACT_INPUT` ("an agent's conversation holds files and images;
  a document belongs to an issue or a project").
  A `session_id` that is empty or holds `/`, `?`, `#`, whitespace, a control character or a character that ends a reference in text (`<>"'` and a backtick, `[`, `]`, `|`) is `400 INVALID_SESSION_ID` on all three agent routes.
  Such a file takes no comment, ask, event read or subscriber through `/api/v1/artifacts/{id}/...`, which is `400 ARTIFACT_AGENT_OWNED`.

- `/api/v1/agents/{session_id}/artifacts/{slug}` (GET; cookie, trusted header, or bearer)

  Read an agent's conversation's file or image and its versions by slug.

- `/api/v1/agents/{session_id}/artifacts/{slug}/versions/{n}` (GET; cookie, trusted header, or bearer)

  Download one version of an agent's conversation's file or image, as `/api/v1/artifacts/{id}/versions/{n}` does, with its cache header.

- `/...` (GET; none)

  Serve the dashboard static files.

An event's payload says what its write moved, so a reader can refresh only what it shows (the dashboard's rules are `packages/dispatch/web/src/api/event-refresh.ts`).
`issue.created`, `issue.updated` and `issue.closed` carry `changed`, the fields whose value the write moved (`title`, `status`, `closed_at` for a close or a reopen, `rank`, `priority`, `assignee`, `parent`, `labels`, `route`, `repo`, `components`, `blocked_by`, `external_links`;
on `issue.created` every field the new issue holds a value in), and `ancestors`, the issue's parent chain, parent first up to the root (`[]` for a root).
`thread_logins`, every login with a Replies row on the write's thread once the write is in, is on `comment.created` and `comment.answered`, on an issue message's `message.created` and `message.answered` (a direct conversation has no Replies thread and carries none), on `ask.opened`, `ask.answered` and `ask.resolved`, and on an `issue.updated` or `issue.closed` that moved the status:
for a close or a reopen, the logins with a row on the issue's open asks; for any other status move, `[]`.
`user_state.updated` carries `changed`, which of `pinned`, `last_read_seq` and `dismissed` the write moved, or `["snoozed"]` for a snooze or an un-snooze of one Inbox row (`PUT` or `DELETE /api/v1/me/asks/{id}/snooze`, which append it only when the row moved).
A snooze's payload is exactly `{login, changed: ["snoozed"]}`:
every signed-in caller's event feed on the live socket replays it, so it names neither the ask nor the writer's issue state, and the writer's other tabs and devices refetch their whole Inbox.
`references_changed` names the targets whose inbound edges the write moved that a reader refreshes by name: an issue (`{kind:
"issue", id}`), an ask (`{kind: "ask", id, issue_key?, artifact_id?}`, with the issue or document whose ask list holds its row) and a document or file (`{kind: "artifact", id, ref}`:
its uuid, the node id `GET /api/v1/references` reports for it, and the `dispatch://` address its Referenced by panel is opened on, `ref` absent when its owner names no address), and a block of a document (`{kind: "block", id, ref, artifact_id}`:
its node id `<document ref_key>#<block id>`, its reference and its document's uuid, whose panel lists the edges at its blocks).
Each kind is capped on its own at 100 targets:
past the cap of a kind the event names none of that kind, lists it in `references_changed_truncated_kinds` (of `issue`, `ask`, `artifact` and `block`), still names the targets of every other kind, and carries `references_changed_truncated: true`, which a reader that does not read the kinds takes as every kind cut.
It also names a target whose edge the write kept but whose excerpt it moved:
an edit of an ask's question or a comment's body that changes the head of its text names every target the text still cites, a document version names each target whose stored excerpt it changed and each target a block it changed still cites, and a retitle names the issue's parent and the issues it is blocked by, whose panels show its title.
A document version also names each block it changed or deleted that something cites, since every link to the block shows its text, and a comment's or issue message's write names each target whose link it moved from one of the item's blocks to another.
An issue write that adds or removes a `blocked_by` entry names each blocker it added or removed, whose `referenced_by_count` counts the edge, and an `issue.created` names every blocker the new issue holds.
Each field is absent from an event written before it existed.

## Dispatch topics

Document events publish retained envelopes on `notifications.dispatch.document.<PROJECT>.<slug>.<type>`.
Use `go run ./cmd/natstail -subject 'notifications.dispatch.document.>' -count 1` to print one matching envelope from the `envoy.natsUrls` configured in `envoy.json`.
`natstail` owns nothing on the bus - it neither creates nor updates `ENVOY_NOTIFICATIONS` - and refuses a NATS server that is not this machine's:
prefix the command with `ENVOY_ALLOW_REMOTE_NATS=1` where `envoy.json` names a shared server, as an agent devbox's does.
It connects as the nkey user `NATS_NKEY_SEED_FILE` (or `NATS_NKEY_SEED`) names, never `envoy.natsSeed`, so a run against a NATS that requires a user sets `NATS_NKEY_SEED_FILE` too.

## Document edit operations

`POST /api/v1/artifacts/{id}/edits` accepts an `ops` array.
Quote targets resolve against the document's plain text rather than Markdown source:
inline-code and link text match without their syntax, and a table-cell target is its cell text.
A quote holding a table with a row shorter than its delimiter row, the header included, is matched as the text it is.

Each item below names an operation and its required fields, then its behavior.

- `replace` (`find`, `with`)

  Replace one plain-text target.

- `delete` (`find`)

  Delete one plain-text target.

- `insert` (`markdown`, exactly one of `after` or `before`)

  Insert a sibling block before or after a quote or heading's enclosing document block.
  `"start"` and `"end"` select document edges.
  Pipe-table row fragments at a table-cell target are the exception: they extend that table before or after the containing row.
  Use `replace` for inline continuation.

Table-row fragments contain body rows only: omit the table header and delimiter row.
Short rows are padded to the table width while all operations in the edit request add at most 10,000 cells;
a larger request is rejected as `INVALID_OP` on `markdown`.
A row holding text in a cell past the table's width is rejected as `TABLE_WIDTH`; blank cells there are dropped.

## Element weights

One write's markdown, and the markdown a document stores, make at most 65,536 elements (`pmdoc.MaxDocumentElements`), weighed node by node over the tree goldmark parses, front matter apart.
Every route that takes caller markdown refuses past that with `413 CAP_EXCEEDED`, and its message lists these weights, written once in `pmdoc/elements.go` (`weighed`):

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

Every node the Proof tree makes other than a text is an element of the live document carrying attributes, and costs the server about what two inline nodes do;
each node class that weighed less let the heaviest document of it the limit admitted hold past the 256 MiB one request may, until it was weighed as above.
`TestEveryNodeOfTheSchemaWeighsWhatItMakes` holds every node type of the schema to at least two elements for each element its cheapest markdown makes and one for each text, and fails for a node type the schema gains until it is given that markdown and a weight.
What each node type weighs:

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

Each item below names a status and code, then its meaning.

- `400 NUL_CHARACTER`

  Caller text holds a NUL character (U+0000), which PostgreSQL's text and jsonb cannot store:
  any string in a JSON body a route decodes, member names included, a multipart upload's field or markdown file, or any route's path or query parameter.
  It covers every parameter, field and member a request carries, including ones the route does not read.
  The message names where it stands (`title`, `options[1].label`, `file`, `path parameter session_id`, `query parameter label`), with a NUL in a name the caller wrote shown as `\u0000`, and the character's position, counted from 1 in UTF-16 units; nothing is written.
  A NUL a browser edit puts in a live document is written as U+FFFD, which is what CommonMark reads one as, in the document's version and every read, a block's id included, an anchor's quote and an ask block's question and options, and a quote, a `# Title` quote and a `heading:` anchor are matched against that text.

- `400 INVALID_UTF8`

  Caller text holds a byte that is not UTF-8, which PostgreSQL's text cannot store:
  a multipart upload's field, its markdown file or a binary file part's `Content-Type` (`file Content-Type`), or any route's path or query parameter (`%FF`).
  JSON cannot carry one, since decoding writes it as U+FFFD.
  The message names the field, the byte and its position, as `NUL_CHARACTER` does; nothing is written.

- `400 ARTIFACT_INPUT`

  An artifact upload's multipart body the parser cannot read, such as a part header holding a control character, named with the parser's reason.
  A body past the upload's size limit stays `413 CAP_EXCEEDED`, and a file part the server cannot spool to its temporary directory is `500 INTERNAL`, logged.
  A markdown document uploaded to an agent's conversation is refused with this code too:
  it holds files and images, and a document belongs to an issue or a project.

- `400 INVALID_SESSION_ID`

  An agent artifact route's `session_id` is empty or holds `/`, `?`, `#`, whitespace, a control character or a character that ends a reference in text, so no reference could name what it owns; nothing is written.

## Document errors

Each item below names a status and code, then its meaning.

- `404 TARGET_NOT_FOUND`

  The quote requested by an anchor or document edit is absent, or the block id a block read (`GET /api/v1/artifacts/{id}/blocks/{block_id}`) names is not in the live document.

- `409 TARGET_AMBIGUOUS`

  A quote matches more than once without an `occurrence`; the response includes candidate ranges and context.

- `400 TARGET_SPANS_BLOCKS`

  A document edit quote crosses textblock boundaries.

- `400 TABLE_WIDTH`

  A table-row fragment holds text in a cell past its target table's width; blank cells there are dropped.

- `409 ANCHOR_MISSING`

  A browser submitted a `mark_id` that the server did not observe in the live tree, in the second it waits for the document's next update and in one last read of the document when that wait ends.
  A mark no ask or comment row records is swept from the document a minute after a settlement first saw it.
  A mark whose `by` names a user with a browser on the room is kept instead, so a composer left open sends, and a mark a closed or offline tab left behind goes a minute after that user's last browser left the room.

- `409 ANCHOR_ORPHANED`

  An operation needs a mark whose anchored text has been deleted.

- `400 INVALID_ANCHOR`

  An anchor must provide exactly one of a nonempty `quote` or nonempty `mark_id`, with its document artifact.

- `400 INVALID_MARKDOWN`

  Uploaded document content cannot be represented by the Proof schema, such as a table row holding text in a cell past its delimiter row's width, which a pipe inside code or a link that is not backslash-escaped makes.
  A Markdown document nests at most 100 blocks (quotes, lists and their items, typed blocks, and footnote definitions), of them at most 32 typed blocks inside one another whatever other blocks stand between them, and one textblock's inline markdown at most 100 marks (emphasis, strong, strikethrough, links, images, and code);
  deeper content is refused naming the line.
  An accepted suggestion whose blocks nest within those bounds but land deep enough that the document would nest past one is `INVALID_OP` on `replace_with`, naming how many blocks, or typed blocks, the result nests.
  Malformed edit replacements report `INVALID_OP`.
  An upload that moves an open anchor's quote where that anchor cannot stand - an ask's quote into a code block, or any open ask's, comment's or suggestion's quote into the front matter - and leaves no copy where it can is refused naming the anchor, its quote and where the text landed:
  move the text out, or resolve the anchor first.
  A copy there beside the original is taken, and the anchor stays on the original.

- `409 DOC_SCHEMA`

  A stored tree is outside the Proof schema, such as one holding a node more than 1,000 levels below the document, a node attribute value nesting more than 100 arrays and objects, or a mark attribute value nesting more than 99;
  settlement writes no version of such a tree.
  Every route that starts from that tree answers it alike, with one message - reading its text or blocks (`/text`, `/blocks`), editing it (`POST /edits`) and naming a version (`POST /versions`) - and a schema refusal of a tree the request itself produced is a `500`.
  Replace it by uploading markdown under the same document name;
  an upload preserves every open ask block, and is refused `400 INVALID_ASK_BLOCK` for an ask block that breaks the ask content rule, as a new document is.
  The document websocket admits no connection to such a room:
  it completes the upgrade and closes with code `4409` (reason `DOC_SCHEMA`, `DOCUMENT_SCHEMA_CLOSE_CODE` and `DOCUMENT_SCHEMA_CLOSE_REASON` in `packages/contracts`, generated into Go) before any sync, so a browser editor never receives a tree it would normalize and write back.

- `409 DOCUMENT_UNLOADABLE`

  A document's stored history does not decode, so nothing can read or open it.
  Rebuild it from its latest saved version (`POST /api/v1/artifacts/{id}/rebuild`); the dashboard offers that rebuild for this code alone.
  A room or store that could not serve the document stays `503 DOC_SERVICE_UNAVAILABLE`, which is retried, not rebuilt.

- `409 DOCUMENT_LIVE`

  A rebuild found the document resident in this server:
  open in an editor, or left by its last editor less than 90 seconds ago (a room stays resident a minute after its last peer leaves, and the sweep that evicts it runs every 30 seconds).
  Close its editors and retry two minutes later, or repair a schema-invalid tree by replacing it from markdown.

- `409 DOCUMENT_LOADS`

  A rebuild found that the document's durable history loads.
  Nothing was deleted; replace a schema-invalid document from markdown instead.

## Comment errors

Each item below names a status and code, then its meaning.

- `400 INVALID_COMMENT`

  A reply must target a thread-root comment.

- `403 NOT_AUTHOR`

  Only a comment's author may edit it.

## Tests

```sh
cd packages/envoy
export DISPATCH_TEST_DATABASE_URL='postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable'
go test -timeout 20m ./internal/dispatch/api
go test -timeout 20m $(go list ./internal/dispatch/... ./cmd/dispatch/... | grep -v '/internal/dispatch/api$')
go vet ./...
```

`internal/dispatch/api` alone takes close to `go test`'s default ten-minute package deadline (about 600 s), so it runs first and on its own, with 20 minutes, as CI's `envoy-go` job runs it (`docs/testing.md`, "Go's package deadline").
