# Changelog

## [27.0.0] - 2026-11-01

### Added

- The secrets broker serves the page a person approves a secret request on, and a secret request
  is approved nowhere else. `GET /approve/{record}` prints the broker's own record of a request
  and every other one waiting on the signed-in person, with Approve and Deny posting back to
  `POST /approve` from the broker's own origin alone; a person signs in to the broker itself with
  OpenID Connect through an OAuth client of the broker's own (authorization code with PKCE S256,
  a nonce and state, no refresh token kept), and the broker keeps that sign-in as a session of
  its own in Postgres, 24 hours at most and never extended. `BROKER_SIGNIN_ISSUER`,
  `BROKER_SIGNIN_CLIENT_ID`, `BROKER_SIGNIN_CLIENT_SECRET_FILE` and `BROKER_SIGNIN_DOMAIN` are
  required: a broker without one of them does not start and names the ones it misses.
  `BROKER_SIGNIN_SESSION_SECONDS` sets how long a sign-in lasts, 86400 by default and at most.
  `GET /signin/status` reports, to a caller with no credential and in words from two closed sets,
  what the hourly check of the broker's client at its provider found and when, when a person last
  signed in, when a sign-in last failed and the coded reason, and when a decision was last made
  on the page. `envoy-broker end-sessions <login>`, run in a broker pod, ends every browser
  session that login holds. `POST /v1/credential-requests/{record}/approve` and `/deny` now
  answer `403 DECIDE_ON_BROKER_PAGE`, naming that page, for every caller when the record is a
  secret request, before any credential is checked, and `agent-secrets-devrelay` has lost
  `approve` and `deny`, the two commands the broker refuses. A machine login is decided exactly
  as before, by an account `BROKER_API_ACCOUNTS` lists sending the login's typed code from
  Dispatch's machine-login page, `BROKER_MACHINE_LOGIN_URL` is still required and still a machine
  login's `verification_uri`, `agent-secrets-devrelay machine-approve` still decides one on the
  local stack, and revokes of grants and machine logins are unchanged. `agent-secrets` prints the
  broker's `approve_url` while it waits for a secret, as
  `waiting for <approver> to approve it on the secrets broker: <url>`; against a broker that
  names none it prints what it printed before.

## [26.2.0] - 2026-11-01

### Changed

- A Dispatch migration added from now on is named for the UTC moment its author created it,
  `YYYYMMDDHHMMSS` (`date -u +%Y%m%d%H%M%S`), with its census under the same timestamp, instead
  of the next free number: two pull requests open at once each took the next number, nothing
  conflicted in git, and the clash showed only once one of them merged. The numbered versions
  keep the numbers they landed with, a timestamp orders after every number, and a migration that
  merges below one a database has already applied still runs there exactly once, since the
  runner applies every version `schema_migrations` does not record. A migration must therefore
  not depend on its order relative to one written in a concurrent pull request: a deployed
  database applies each one as the release carrying it deploys, so it can apply them in the
  other order than a fresh database does. `schema_migrations.version` is now a `bigint`, which a
  14-digit version needs; a database that has the table as an `integer` keeps it until the first
  boot whose set holds a timestamped migration, which widens it once, in the locked transaction
  the runner creates the table in, so the previous release's health probe keeps answering during
  this one's roll. Roll this release out before any release that carries a timestamped
  migration. `schema_version` in `/healthz` is unchanged, the highest version applied rather
  than proof that every version below it is applied; the auto-deployer's rollback guard now
  compares the whole set of versions `schema_migrations` records, and records it in
  `compose/.env` as `ENVOY_IMAGE_SCHEMA=<tag>:<max>-<count>-<digest>`.

## [26.1.1] - 2026-11-01

### Changed

- Dispatch's change-driven background loops send no statement while nothing is written. The
  embedding queue (which scanned every 5 s), the reference excerpt reconciliation (every five
  minutes, and every 2 s while a batch came back full), the suggestion outcome sweep (every
  minute) and both GitHub intakes' delivery settings (read every 30 s, and by the job intake on
  every message) now run a pass at start and then only when a Postgres notification says their
  work may have changed or a due time they asked for comes round, never more often than the timer
  each replaced. Each process holds one more Postgres connection for this, a shared listener
  logged as `dispatch background wakes`, with its keep-alive ping; it signals every loop after
  each LISTEN, so a change made while it was reconnecting is caught up then. The embedding
  coverage pass and the task progress reconciliation run once at start instead of every 10 and 5
  minutes: coverage repeats only its identity steps, and only while another embedder identity's
  rows remain, and task progress retries the rows another writer held for about a minute. The
  delivery reconcile, the architecture import and the webhook redelivery sweep keep their timers;
  all three now draw their jitter afresh every period, so two processes' passes drift apart. The
  `Idempotency-Key` rows past their 24 hours and the deliveries a process that died left owed,
  which the suggestion outcome sweep's pass carried, keep a once-a-minute leased timer of their
  own (`api.RunMaintenanceSweep`, still under the `suggestion-outcomes` lease, so one process
  sweeps per minute however many serve the database): no notification announces an expired key
  or a dead process's owed delivery (`cmd/dispatch/README.md`, "Background loops").
- Migrations `0151`–`0156` back those loops: `embeddings_enqueue` and `delivery_settings_changed`
  notify `dispatch_embeddings` and `dispatch_delivery_settings`, a new row trigger on `refs`
  notifies `dispatch_excerpts_owed`, and `reference_excerpt_backoff`,
  `suggestion_outcome_cursor` and `write_suggestions.swept` (with two partial indexes) are new.
  All are additive, every census answers `0`, and the release before them keeps working beside
  them during a roll. The suggestion outcome sweep moves `suggestion_outcome_cursor` only up to
  the event log's safe horizon, the id at or below which every event is final, as the live event
  feed reads: event ids are not commit order, so a cursor past an event that could still commit
  would never judge by it.
- A delivery settings change (the deploy repository, the workflow paths) reaches the running
  intakes as soon as it commits, through the shared listener, instead of within 30 s.
- The GitHub intake handles up to four pull requests or workflow runs at once, its consumer's
  `MaxAckPending`, where it handled one message at a time: messages about one pull request or one
  run still run in order, one after the other, so an older GitHub fetch is never written over a
  newer one. A message whose subject cannot carry a pull request or a run (checks, comments,
  pushes, jobs, `pr.<n>.review`) is acknowledged without decoding it. The consumer's `AckWait`,
  `MaxAckPending` and filter subject are unchanged.
- When the GitHub intake stops, it drains its subscription, the one its bus client bound again on
  a new connection when the client replaced its connection, and holds the messages queued behind a
  busy pull request or run, and those NATS hands it meanwhile, until no handler is running. The
  messages in a handler get up to 15 s from the stop to finish and be acknowledged; the handlers
  still running then are given up, their GitHub calls cancelled, and their messages handed back to
  NATS. Only then are the held messages handed back, so the next holder never runs a pull
  request's or run's next message while this process still runs the one before it. The intake
  then waits for the drain to end and releases the subscription, so a reconnect does not bind the
  stopped one again. Dispatch's shutdown waits for that stop, which begins at the signal, before
  it closes its database pool, so a handler GitHub answers during the stop writes what it fetched
  rather than failing on a closed pool with its message acknowledged (`cmd/dispatch/README.md`,
  "Background loops").
- A document whose reference excerpts cannot be read or stored is retried one minute after its
  first failure, doubling to at most six hours, plus up to a fifth again of jitter, instead of on
  every pass, and is left out of the batches meanwhile, so documents that keep failing no longer
  hold the reconciliation at its 2 s step nor keep the documents after them waiting. The
  excerpts a pre-`0092` build left stale are marked owed once per process at its first pass,
  rather than on every pass for the hour after start.
- The suggestion outcome sweep reads only the events written since its last pass and the
  suggestions recorded since, so its work follows what changed rather than how many suggestions
  are still pending; it judges a suggestion once the suggestion and the events that resolve it are
  a minute old, and the first pass on a database judges every pending suggestion once, as before.
- The edit notice sweep sends no statement while no notice is due. It swept `document_edit_notices` every 5 s; it now runs a sweep at start and then only when a new migration, `0157`, announces a notice given a due time on `dispatch_edit_notices` (a row trigger, so notices the release before writes during a roll wake it too), or when the earliest due time it read comes round. Sweeps stay at least 5 s apart, so a notice still goes within 5 s of the end of its quiet window. The census answers `0`, and the release before keeps sweeping on its timer beside it (`cmd/dispatch/README.md`, "Background loops").
- The webhook redelivery sweep still sweeps at start, but then waits two minutes plus up to a tenth again after each sweep, drawn afresh every period, as the delivery reconcile and the architecture import do, in place of a plain two-minute ticker, so two processes started together no longer reach its lease in step every two minutes.

## [26.1.0] - 2026-11-01

### Added

- A Google Compute Engine VM logs in to the secrets broker and enrolls itself with the identity
  token its metadata server signs, with no stored credential and no person approving it. With
  `BROKER_GOOGLE_OIDC_AUDIENCE` set, the broker discovers Google's issuer at startup (one
  `google vm identity` log line), and `POST /v1/launcher-credentials/token` also takes a VM's
  token (`format=full`) whose account `BROKER_VM_SERVICES` binds, from the project
  `BROKER_VM_PROJECTS` names and with an instance name `BROKER_VM_INSTANCES` matches; anything else
  is `403 ACCOUNT_NOT_LISTED` naming what did not match, or `401 TOKEN_INVALID`. Each token goes to
  the one verifier whose issuer it names, so a VM's token never makes the broker read the
  cluster's keys, nor a cluster token Google's. Each VM holds its own launcher credential and its
  own `vm` enrollment, keyed by `<account>@<project>/<zone>/<instance id>`, so VMs that share an
  account never move or end each other's. No launcher credential ends a VM's enrollment
  (`DELETE /v1/enrollments/{id}` is `403 VM_ENDS_BY_LOGIN_REVOKE`), so a token copied off the VM
  cannot end or take over its live enrollment; revoking the VM's machine login, or its lease,
  ends it. A revoke does not revoke the copied token, which logs in again within its hour; taking
  the VM out of the fleet's settings ends that. `POST /v1/enrollments` takes kind `vm` with `vm_token`
  (`403 VM_IDENTITY_MISMATCH`, `403 VM_TOKEN_EXPIRED`), every renewal of one carries the VM's
  current token (`403 VM_TOKEN_REQUIRED` without it), and a VM service's secrets go to its own VMs
  at once. A VM its fleet's settings no longer allow (its account dropped, or another project or
  instance-name glob configured) has no identity from then on: every call it signs is refused
  `401`, as an ended session's is, so it releases no grant it holds, opens no request and renews
  nothing, and a person's approval of a request it left pending grants nothing; its launcher
  credential no longer authenticates (`401 LAUNCHER_INVALID` on a new enrollment), and its next
  login is `403 ACCOUNT_NOT_LISTED`. On the client, `agent-secrets enroll --vm --service <name>
  [--audience <aud>] [--out <dir>]` fetches the token, logs in with a key it keeps in memory,
  enrolls `key.pem`, and prints the enrollment id; run again over the same directory it keeps
  `key.pem` and extends the lease. `agent-secrets renew --vm` renews with a fresh token each time
  and stops (exit 76) on `VM_TOKEN_REQUIRED`, `VM_NOT_LISTED` or `ENROLLMENT_NOT_LIVE`. Both ask
  the metadata server directly, never through a proxy `HTTP_PROXY` names, since the token logs
  in as the VM. With no
  Google audience nothing changes: kind `vm` is still `400 INVALID_KIND`, and the Kubernetes and
  GitHub paths are untouched.

## [25.2.0] - 2026-11-01

### Changed

- A Dispatch document edit repeats less work for the same answer: a tree the edit touches is
  validated once for the batch's reads that take its validated value rather than once a read,
  every quote of a batch is resolved against one flattened text of its tree, the document-level
  blocks a write changed are found in one walk and read by every refusal that needs them, and the
  quote finder's text is built in buffers of its own size.
  The reads of a write transaction's fork before and after its write go through the lock-free
  reader, without ygo's ToDelta, and the renders of the write's own operation take the tree it
  validated rather than validating it again. A conditional edit encodes its tree once: the token
  its precondition check computed is the token its batch starts from. Every answer, token,
  version, anchor and CRDT update is what it was.

## [25.1.0] - 2026-11-01

### Added

- Dispatch takes a fifth credential: an access token the secrets broker issued, presented under
  RFC 9449's DPoP scheme (`Authorization: DPoP <token>` with a `DPoP:` proof header). The server
  decides a request by its authentication scheme before it reads any token, so such a
  presentation is never retried as a bearer and the bearer credentials are untouched. Dispatch
  asks the broker what the token is and holds the caller to the key it is bound to, and the
  caller writes as the session that token names, acting for the person it names: a body with no
  `actor` is accepted, one naming that same session is accepted and its `origin` kept, and one
  naming any other writer is `403 ACTOR_SESSION_MISMATCH` naming the session the token carries.
  A token scoped to one project carries that project on every actor it writes, set from the
  token and never from a request body; one scoped to its person carries none. Each refusal has
  its own code and none repeats the token: `401 BROKER_TOKEN_NOT_ACTIVE`,
  `401 BROKER_TOKEN_PROOF_INVALID`, `401 BROKER_TOKEN_KEY_MISMATCH`,
  `401 BROKER_TOKEN_NOT_A_SESSION` for a live token that is no session's credential — one the
  broker names no session or person for, or that it issued to a client other than the exchange,
  which a machine login's own access token is — so a request body can never name the session a
  credential does not prove, `429 BROKER_TOKEN_RATE_LIMITED` with the wait in `Retry-After`,
  and `503 BROKER_UNAVAILABLE` for
  a broker that answered nothing, so a caller comes back rather than reading its token as
  refused. The verifier is built from settings Dispatch already has —
  `DISPATCH_AGENT_SECRETS_URL`, `DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE` and
  `DISPATCH_SERVER_URL` — and adds none; a deployment with no broker has no such credential,
  and a DPoP presentation there is the `401 UNAUTHORIZED` any Authorization header that is not
  a bearer has always been.
- The broker: a pod exchanges its projected token (RFC 8693) under a session name only while a
  registration says that pod may. A deployment's own daemon writes it as it starts the pod —
  `POST /v1/oauth/exchange/pod-sessions`, `{"pod_token", "session"}`, authenticated by the
  account `BROKER_SERVICE_LOGINS` lets log in as a service, the list it already holds for its
  token login — and the exchange reads it in its own transaction, asking the cluster nothing. The
  session name still belongs to the listed service account, so a registered successor keeps what
  its predecessor held and a relaunched pod is not locked out; what the registration adds is the
  one thing the claim on a name cannot do, which is tell two pods of one account apart, so a pod
  naming a session of another issue of its own project is now `invalid_grant` naming that
  session. An entry that fixes a session needs no registration: its name is the configuration's
  and every pod of that account writes under it, so the replacement pod of a fixed-session
  account and the next run of a fixed-session workflow are answered as before. A registration is
  kept 24 hours past its last write, the longest a presented pod token can live, and the
  authority is the registrar's: a daemon that stops registering costs its pods their writes
  within that window.
- Dispatch: a credential that names one project now writes that project and nothing else. Every
  mutating route declares what its write belongs to — a project's (the issue-key, project-key,
  artifact-id, ask-id and comment-id rows, and the message rows that carry an issue), a project
  the body names (the issue create, the one write whose address names no project, whose handler
  makes the same check the moment it has read one), the
  calling session's (an agent conversation's own artifact and messages, the two routes a session
  answers its own delivery on, and the caller-private `me` rows), or the deployment's (the
  project create, the repository-to-project map, the delivery settings and session groups, the
  broadcast, and the credential and machine-login decisions) — and two checks in front of every
  one of them decide it. The project check resolves the project the addressed object belongs to
  and lets through only a target of the caller's own project: another project's, one that
  belongs to no project, and one that resolves to nothing at all (no such row, or an id
  Postgres will not read as a uuid) are each `403 PROJECT_SCOPE`, naming the project the
  credential covers and the project the write belongs to where one resolved, and nothing of the
  credential; a deployment-wide write is `403 PROJECT_SCOPE_DEPLOYMENT`, naming that project and
  the route. Refusing a target it cannot resolve is what keeps the check closed, and it says
  nothing about which ids exist outside the caller's project. The session check holds the rows
  whose path names a session — an agent conversation's writes and the caller's own agent view
  state — to the session the caller's credential proved: another session's is
  `403 SESSION_SCOPE`. Both are a no-op for every credential that carries no project and proves
  no session — a browser cookie, a personal agent token, a Kubernetes service-account token, the
  shared token, and an access token the secrets broker scoped to a person rather than to one
  project — so no caller that exists today is affected; that is the rule and not a transitional
  measure.
- The broker ends one of its own access tokens on demand: `POST /v1/oauth/revoke`, OAuth 2.0
  token revocation (RFC 7009), whose form names the `token`, optionally which kind to look for
  first (`token_type_hint`), and the `client_id` it was issued to —
  `dispatch-token-exchange` for a pod's or a workflow run's exchanged token, the machine-login
  client for a dev box session's. It is the only way an exchanged token can be ended before its
  own expiry, since such a token carries no credential id for a credential revocation to reach.
  The route takes a listed caller's own Kubernetes ServiceAccount token and needs the power that
  relays a person's decisions, so `dispatch` reaches it and `listener`, listed to read a token
  back, is `403 ACCOUNT_LACKS_POWER`; a token issued to another client than the request names is
  `400 unauthorized_client`, and a token the broker never issued is `200` exactly as a token it
  just ended is. A revoked access token is `active: false` to `POST /v1/oauth/introspect` from
  the moment the revocation commits, so a resource server that cached an affirmative
  introspection refuses it within that cache's life and no longer. Ending an access token ends
  that token alone: the machine login behind it still refreshes, and only a caller presenting
  that login's own refresh token ends the whole family and the login with it.

### Changed

- The broker: a caller's name in `BROKER_API_ACCOUNTS` now decides which of its named-caller
  routes that ServiceAccount reaches, where being listed once reached all ten. Each such route
  declares the power it needs — reading one of the broker's access tokens back
  (`POST /v1/oauth/introspect`), or relaying a person's decisions (the nine approval, denial,
  revocation and listing routes Dispatch's server calls) — and the broker's own table says which
  powers each caller name carries: `dispatch` both, `listener` introspection alone. A listed
  caller reaching a route its name does not carry is `403 ACCOUNT_LACKS_POWER`, naming the caller
  and the route; an account the list does not name at all stays `403 ACCOUNT_NOT_LISTED`. A
  `BROKER_API_ACCOUNTS` entry whose caller name the broker does not know refuses the boot, naming
  that name and the names it knows, so a typo cannot quietly produce a caller that reaches
  nothing.
- `GET /api/v1/whoami` answers `session` and `project` beside `owner` and `service`: the session
  the caller's credential itself proved, which only an access token the secrets broker issued
  does, and the one project key that credential may write. Both are null for every other
  credential, which proves neither, and every existing field keeps its meaning.

### Removed

- The document editor's live connection (`GET /ws/doc/{room}`) no longer takes a bearer. It was
  a second, weaker door into Dispatch: the shared agent token alone opened it, and the acting
  session came from a header the caller wrote. The socket now resolves a person through the
  identity service and nothing else, which is how the browser has always connected (a
  same-origin cookie); no caller outside Dispatch's own tests presented a bearer to it.

## [25.0.2] - 2026-10-31

### Fixed

- Dispatch's `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES` descriptions now state the real bound between the warning `envoy nats reconnect hook failed; it is retried` and the error `envoy nats reconnect hooks have not succeeded`: Dispatch reconciles the stream on its first connection, retrying until that reconcile succeeds, the minute runs from that first connection to NATS, not from the first retry, and `stall_ms` on both lines is how long the reconcile has been due.

## [25.0.1] - 2026-10-31

### Fixed

- A document whose formatted text a write or a settlement repair had partly removed no longer reads back with its formatting changed (`*x*y` as `*xy*`) in a read, a version or after its room reloads. Since the ygo pin moved to v1.54.1-sami.1, ygo cleaned up the format markers after every update it applied as remote, and the room's reads and the fold of its stored history (which compaction stores) applied the room's own writes that way, where the room itself had run no cleanup. Dispatch's ygo pin moves to v1.54.1-sami.3, which adds a switch to turn that cleanup off (reearth/ygo#369, open upstream), and every copy of a room now replays the room's writes with it off. A room's load also applies its stored history with the cleanup off, so a loaded room holds the state its history holds. The room still cleans up after a browser's edit and a write's publish, and stores that cleanup.
- A comment, suggestion or ask written on a document keeps its anchor however long the composer
  stays open. The provisional mark a composer writes is no longer swept while its author still
  has the document open, and its one-minute life is counted from the author's last presence, so
  an idle reconnect no longer costs the mark while a tab that closes still loses a stray one a
  minute later. Send writes the mark again over the reader's own words when it is gone anyway,
  then sends once more under a key of its own: it finds them by the mark itself, by the text
  around them in the block they were selected in, or by the one reading the whole document
  surrounds the same way, a selection that runs across a paragraph break included. Words that
  read in more than one place with nothing to tell them apart attach nothing, and each refusal
  says which case it is - the words were changed or deleted, they read elsewhere but not where
  they were selected, the text around them changed, they read alike in more than one place, or
  their document is not open - and keeps the draft. An agent's client still reads the server's
  `ANCHOR_MISSING` wording unchanged.

## [25.0.0] - 2026-10-31

### Changed

- `envoy-infra`'s `gcp:Dispatch` now declares Dispatch's Tailscale Service `svc:<hostname>` itself,
  after the Cloud SQL instance, so the tailnet name comes back only once an availability-type
  switch has restarted the database. Breaking: the `tailnetService` input is gone and a consumer
  passes a `tailscale` provider instead. The component's create is an upsert, so it adopts a
  Service that already exists under that name and overwrites its comment, ports and tags: a stack
  that declared `svc:<hostname>` itself hands it over in two applies, `retainOnDelete: true` on
  its own declaration first (with `protect: false` on it if it is protected) and the pin move
  second, since removing that declaration without the retain deletes the Service the component
  has just adopted, and `pulumi state delete` is refused there because the component depends on
  the Service's output. The front door's Deployment waits for the Service and the
  instance; the server Deployment no longer waits for the front door, so it comes back inside a
  cutover's stop, while the listener's still does. A new `databaseWaitsFor` input orders the
  instance's update after a resource the consumer names. A new `signingKeyGenerations` input makes
  `DISPATCH_SIGNING_KEY` an ordered key set, so an operator rotates the session signing key in
  three applies — `[0]`, `[0, 1]`, `[1, 0]`, `[1]` — without signing anyone out; the default `[0]`
  renders exactly the one key the component always wrote.

## [24.8.1] - 2026-10-31

### Changed

- A soft role claim on `POST /v1/roles/set` that names a `previous_session_id` takes the role from that predecessor only when the predecessor has no live registration, when its live registration carries the same `client_instance_id` as the claimant, or when either side carries no instance id. Any other live predecessor keeps the role, and the claim is answered 409 naming it. The predecessor's registration is read from the `envoy_sessions` bucket itself rather than from the cache of it, and the claimant's is read from the bucket too, falling back to the cache when that read does not answer. `POST /v1/interests/subscribe` stores the new optional `client_instance_id` field on the session entry, and `DELETE /v1/sessions/<id>` accepts it as a query parameter: with it, the registration is removed only while it carries that id or none, and the answer is `200 {"deleted":true}` or `200 {"deleted":false,"reason":...}`.

## [24.8.0] - 2026-10-31

### Changed

- A document left open and idle keeps its connection through ygo v1.54.1-sami.1's awareness echo, as `@hocuspocus/server` does: an editor's browser gets its own presence renewal back, and a read-only page gets a small empty presence reply. Dispatch no longer writes keep-alive frames of its own into the document socket.

### Fixed

- Dispatch's ygo pin moves from v1.51.3-sami.4 to v1.54.1-sami.1, the sjawhar/ygo fork rebuilt on upstream ygo v1.54.0. For people editing a document this fixes:
  - An edit the room refuses now changes nothing. Before, an edit refused for holding too many items that wait on an earlier edit had already put its first part in the document, which was saved while the sender was told the edit was refused and the other editors never got it. Deletions that wait on an earlier edit are now capped the same way.
  - An edit the room takes only in part now reaches the other editors with what it added and with what it holds until an earlier edit arrives. Before, they did not get that part and stayed behind the room until they reconnected.
  - A browser that stops reading is closed when a write to it fails, and the room no longer counts it as present. Input it sent before the server closed it is dropped, and the browser sends it again when it reconnects.
  - A link or other mark on observed text no longer hangs the document on the next edit, and a browser that sends its unacknowledged edits again after a reconnect no longer fills the room's queue of waiting edits.
  - A crafted edit with a clock past 2^53-1 or a client id past 2^55-1, which no Yjs browser makes, is refused. A stored document that already held one would no longer load; browser and server client ids are 32-bit, and no stored document checked holds one.

## [24.7.0] - 2026-10-31

### Added

- Broker migration 0020 lets the `enrollments` kind check admit `vm`, the enrollment kind of a
  Google Cloud VM, beside `session` and `pod`. It recreates the one check constraint and rewrites
  no row, so a broker of the release before it keeps serving over the migrated database, and a
  rollback past it needs no down migration. `POST /v1/enrollments` still takes `session` and `pod`
  alone; nothing creates a `vm` enrollment yet. The migration holds `ACCESS EXCLUSIVE` on
  `enrollments` for the instant its transaction lasts, so a request of the earlier broker that
  reads `enrollments` waits that instant. Behind a long transaction it waits at most the runner's
  5 s lock timeout, after which the new broker's boot fails having applied nothing, and its next
  boot tries again.

## [24.6.0] - 2026-10-31

### Added

- `envoy-infra:gcp:Dispatch` takes its tailnet inputs and its sign-in inputs as two groups, each
  set whole or left wholly unset. With the tailnet group — `tailnetDnsName`, `tags`,
  `tailnetAuthKey`, `tailnetService`, `tailscaleImage` and `frontDoorToolsImage` — unset the
  component builds no front door at all: no `<hostname>-tailnet` Deployment, Service,
  NetworkPolicy or ServiceAccount, no `<hostname>-tailnet-auth-key` or `<hostname>-tailnet-tls`
  Secret, neither RBAC object for that Secret, and no `<hostname>-serve` ConfigMap. The `url`
  output and the app's `DISPATCH_SERVER_URL` are then
  `http://<hostname>.<namespace>.svc.cluster.local:8766`, so Dispatch is reached inside its own
  cluster alone. That mode requires `natsInCluster` and at least one `allowedNamespaces` entry,
  and refuses by name `frontDoorReplicas`, `frontDoorPdbMinAvailable` and every sign-in input,
  since browser sign-in needs a redirect origin an OAuth provider takes. With the sign-in group
  — `signInClientId`, `signInClientSecret` and `signInDomain` — unset the app container carries
  no `DISPATCH_SIGNIN_*` variable and the Secret `<hostname>-dispatch` holds `DATABASE_URL`,
  `DISPATCH_AGENT_TOKEN` and `DISPATCH_SIGNING_KEY` alone. A group set in part keeps its
  per-input refusal, with one clause naming every input of that group still unset. A stack in
  this mode needs an `envoyImage` built from a sjawhar/legion `main` commit at or after this
  change: an older image's `envoy-dispatch` exits 1 when it is given no `DISPATCH_SIGNIN_*`
  setting.

### Changed

- `envoy-dispatch` boots with no `DISPATCH_SIGNIN_*` setting at all: it runs no browser sign-in,
  nobody signs in, and it serves its agent-token API as it always does, answering
  `GET /auth/start` `503 SIGNIN_UNCONFIGURED`. Such a server also refuses every `dsession`
  cookie, one a sign-in minted before the settings were dropped included: every request that needs a
  person answers `401 NO_IDENTITY`, since the deployment's own `DISPATCH_SIGNING_KEY` goes on verifying
  such a cookie for its thirty days and no issuer is left to confirm that the person it names
  may still use Dispatch. Dev sign-in keeps its cookies: it mints them itself. A deployment that
  sets some of the sign-in settings and not the rest still refuses the boot, naming every
  setting it misses, so a production deployment that loses one of them is refused as before.

### Fixed

- A listener no longer starts serving `GET /v1/sessions` or `GET /v1/registry/<session_id>` before it can report a session's roles, so a client of a healthy bus no longer reads `envoy: the message bus did not answer` from a listener that has just started. Its startup gate waited for the role claim watcher's readiness, which a first scan nats.go's idle timer ended also releases, while the view those two routes read refuses every read until that scan has delivered each claim; the gate now waits for that view to be complete (`kvwatch.Watcher.WaitComplete`, `store.Registry.WaitForCacheReady`). A start that completes no scan still serves, fail open, warning that the warm-up did not complete: a live watcher whose scans keep timing out holds the start for the gate's whole 30 seconds, while a terminal error of that watcher or its stop ends the wait at once.

  The gate covers the start alone. A later `Rewatch` — a reconnect, a self-health rebuild, a recreated bucket — begins a generation of its own, and those two routes answer the same 503 until its scan completes. `/healthz` now says so when it reports no other fault: `200` with `"status": "degraded"`, an error naming the role claim view (`store.Registry.RoleViewErr`, the state `RolesHeldBy` itself reads) and the healthy answer's other fields. It is checked after every check that can answer `unhealthy`, so it hides none of them. It is degraded rather than unhealthy, and no part of `Ping`, because the view fills itself through its watcher's own rescan and a restart completes no scan that keeps timing out.

  The refusal's 503, its `Retry-After: 1` and its body are unchanged.
- The guard that holds every read of `artifact_reviews` to `docs.ReviewOrder` now reads a join as it reads a `from`, so a statement that reaches the table through a join and takes the newest row by `created_at desc, id desc` is refused.

## [24.5.0] - 2026-10-30

### Added

- A Dispatch document write that stored a version replacing the one a person had approved says
  so in its advice: `superseded_approval` names the approved version, the version the write
  stored and who gave that approval. A document edit and an upload that replaced an existing
  document each read the document's newest review, in the order the reviews' events committed
  (`docs.ReviewOrder`), inside their own transaction, after their
  version row and any approval carry, and report only an approval of the version right below
  the one they stored - so the write that closed a Legion design gate is told, while a write on
  a document nobody approved, one whose approval was already stale, one a changes request had
  already closed and one whose version carried the approval forward report nothing.
- `packages/envoy/loadtest/` replays LINUX's real write log of 2026-10-26 22:55-23:34Z against a local Dispatch (`loadgen run -source replay`), so a change to Dispatch's write path is measured on production's writes. `run-step.sh <name> --replay` is the one command: it gets the bundle and the seed the committed manifest (`replay-2026-10-26.json`) names, checking every part's sha256, prepares a template for the Dispatch build once (the seed is at 35efd326's migration and the build migrates it forward), resets to it, and re-sends each logged write at its offset as the dispatch CLI sends it, with read actions before each write at a stated assumption. `--rate`, `--reads`, `--read-mix`, `--uploads`, `--from`/`--to`, `--roster` and `--rng-seed` are its inputs; `--dispatch-commit REV` builds Dispatch alone from another commit, and `--gomaxprocs N` sets its `GOMAXPROCS`. One bundle, one set of inputs and one rng seed send the same requests in every run, and each document's writes are sent in log order (a write waits for the previous one on its document), so three runs at a change's base and three at its head differ only in Dispatch, and `replay.end_state.sha256` shows they left the same documents.

  Each run's `summary.json` `replay` gives Dispatch's CPU-seconds over the window from `/proc`, the cores at the full rate (and with the idle cores taken off), writes planned, sent and failed, substitutes, saturation, the writes that waited for their document's previous write (`doc_order_waits`, `doc_order_wait_s`) and the end state of the documents written (`end_state`, with `end_state.json` in the step directory), and the step directory holds heap profiles at the window's start and end (`go tool pprof -sample_index=alloc_space -diff_base=heap-start.pprof -top envoy-dispatch heap-end.pprof`). `loadgen replay-plan` prints what a run would send without a stack. `loadgen replay-fetch`, `replay-pack` and `stack.sh replay-seed` and `replay-manifest` rebuild the bundle and the seed, whose history versions change as many bytes as production's and whose anchors land by stated rules that `replay-check-seed` checks. `loadtest/README.md`, The replay, has every input, field and what the replay does not reproduce.

### Fixed

- An envoy-listener now declares a NATS connection lost two to three seconds after the hub stops
  answering it, where it read connected for four to six minutes before. A hub whose packets stop
  closes nothing, so the connection looked healthy while every publish on it waited and no live
  message arrived; only Dispatch's server carried the pings that find such a cut. Both of the
  listener's connections, the one that owns the stream and the live topics lane, now ping once a
  second with two outstanding, and a connection declared lost redials as one whose server closed
  it does. A loaded hub whose answer takes more than two seconds is taken for a cut one, which
  costs a redial.
- A KV cache watch that a reconnect's rewatch has already replaced no longer ends at ERROR. The
  listener's caches arm their first watch in the background, and a watch that was replaced while
  it was still starting logged `<cache> watch failed` at ERROR, which pages an operator for a
  watch a repair had already taken over; now it ends with one INFO line naming the rewatch. A
  watch that fails while it is still the live one keeps its ERROR, because then nothing feeds
  that cache.

## [24.4.0] - 2026-10-30

### Changed

- Writes to different issues no longer wait on one database-wide advisory lock that every event
  write took until it committed: a write now locks only its own issue's row (or its document's or
  project's), so one issue's events still commit in order while a slow write on one issue no longer
  holds back writes on every other. Event ids then no longer commit in id order across issues, so
  the live event feed (the events subscription on `/ws/live`, and its resume), the delivery hub and
  `GET /api/v1/references?since=` read only up to a safe horizon, the highest id below which every
  event has committed or rolled back: an event that commits late under a lower id is still
  delivered, in id order, rather than skipped. The cost is latency to live readers: a just-written
  event reaches them once every transaction that holds a transaction id on the Postgres instance,
  in Dispatch's database or any other, when the first observation of the horizon after the
  event's commit is taken has ended. That observation comes after the commit (the live feed's
  relay reads 25 ms after the event's notification), so the event also waits for transactions
  that began after it committed and for ones that write no event (a `SELECT ... FOR UPDATE`,
  another database's job on that instance); a wait over five seconds logs the oldest such
  transaction. A live events subscription that resumes from an id opens at once and heartbeats while the
  horizon is held, a cold one opens once its subscription is in place, and shutdown ends a
  subscription still waiting. The delivery hub is ready and takes delivery changes while the horizon is held,
  and starts acting on events once the first event cursor is proven. A
  `GET /api/v1/references?since=` read waits up to 1 s for the horizon to pass every event
  committed before it; past that it answers the mentions at or below the horizon, and a later read
  the rest. Each observation of the horizon uses one transaction id (none while readers are idle)
  and runs on a two-connection pool of its own, so a full shared pool does not hold live readers
  back. A failed observation is logged (at most every 30 s), and a reader waiting on the horizon
  gets the error back once every observation has failed for 10 s. The horizon is read on the
  primary only. `envoy-infra:gcp:Dispatch` counts the horizon's two connections in each app
  pod's Postgres budget (`dispatchConnectionsPerPod` 89). Deploy the first release with this change
  at one replica, which recreates the pod, and raise the count afterwards: during a roll above one,
  an old pod's live readers can skip an event a new pod commits out of id order.
  A live events subscription with no `since` starts at the relay's published cursor. The relay's first
  cursor in a process is the log's newest id when it starts, or below the lowest id that drew its
  place before then and committed while the start waited, so the subscription is sent every event that
  commits after it subscribed. The first start reads only the ids written since a mark the process
  takes at boot or at the start, so its cost follows recent writes, not the size of the log.
  When an event insert from before boot keeps every mark from counting, it reads above the
  horizon the process before it persisted (the new one-row table `event_horizon`, migration 0149)
  rather than the whole log. GET /auth/whoami answers `503 EVENT_HEAD_UNAVAILABLE` only while no
  event id is proven final yet, which the dashboard asks again for as long as it lasts, showing
  its loading skeleton; it answers `503 EVENT_HEAD_READ_FAILED` when the database does not answer,
  which the dashboard retries twice and then shows as "Couldn't reach Dispatch." with Retry.

## [24.3.1] - 2026-10-30

### Changed

- Every container image this package builds from or starts now comes from
  `ghcr.io/sjawhar/legion/ci`, one package this repository publishes, named by digest: the
  listener image's web, build and runtime stages (`docker/Dockerfile`), the held-port wrapper's
  build stage (`infra/host/heldportwrapper/Dockerfile`), and the servers the Go test helpers
  start (`internal/testpg`'s pgvector, `internal/testnats`'s NATS and NATS hub,
  `internal/tests3`'s SeaweedFS). `.github/ci-images.yaml` holds one row per image, and
  `.github/scripts/mirror-ci-images.sh` copies the rows the package does not hold yet with
  `crane copy`, which keeps the digest, so each copy's digest is its upstream's. No build or
  test of this package reads a third-party registry, and a failed pull is this repository's own
  fault. The developer scripts under `scripts/`, which fetch nothing in CI, and the compose files
  under `deploy/compose/`, which no CI job runs, still name Google's mirror.

### Fixed

- An agent's options edit of a Dispatch decision keeps what a person wrote under an option.
  Each sent option is paired with the list item already holding it, first by the whole option
  and then by its label, so an option the edit does not change keeps its own node — its line as
  it stands, its block id, its marks and the sub-list, second paragraph or link under it — and
  one whose description it changes keeps every block under its line. An edit that would drop
  an option holding more than its plain line, or rewrite a line that holds a link, bold, code or
  an image, is refused whole, with that item or that line quoted and `dispatch doc-edit` named,
  since the ask row shows an
  option's label and description alone. What those refusals weigh is what the markdown writes,
  so a mark no reader sees — the editor's own on every character a person types — is nothing to
  lose. Settlement's restore of a decided ask's recorded wording has no caller a refusal could
  reach, so it drops an item no decided option is paired with, whatever that item holds: a block
  left disagreeing with the row of the ask it was decided on would disagree for good. A decision
  whose question a person emptied is repaired by an ask edit carrying a question, which keeps
  every option list in place; an edit of it carrying no question is refused with that step named,
  where before every edit of it was refused with a sentence that named none. Where that repair
  leaves the block holding more than one list, the ask row keeps the options it holds rather than
  the options of every list joined, which is no decision's reading. A refusal that names blocks
  to delete names a nested decision the content rule leaves nothing of by its own block id, where
  it used to claim no document edit works, and says that an open ask on a decision it names is
  answered or resolved before the delete.

## [24.3.0] - 2026-10-30

### Fixed

- The Envoy listener no longer logs an error for a key-value watch rebuild it repairs by itself: a
  rebuild that fails after a NATS reconnect is run again within seconds, and is a warning until it
  succeeds. A log filter reads two lines in place of the ERROR `envoy nats reconnect hook failed`:
  under a minute the WARN `envoy nats reconnect hook failed; it is retried`, and at or past it the
  ERROR `envoy nats reconnect hooks have not succeeded`, both carrying `name`, `error` and the new
  `stall_ms`, how long the reconnect hooks have been due without a run that succeeded. `/healthz`
  now reports such a stalled rebuild as a degraded answer naming the KV watch rebuild and how long
  it has gone without succeeding; before, a listener answered `healthy` while its caches followed
  key-value buckets it no longer watched. The answer stays a 200 because the container health check
  and the readiness probe read `/healthz` themselves, so refusing would take out the one listener
  still serving the caches it holds and replace it with one that rebuilds into the same failure
  with caches that warm up empty; on AWS the state pages through an alarm of its own,
  `<N>-reconnect-hooks-stalled`, on that error. Both halves of such an outage - the bus's rebuild
  and a watcher that stopped without one - now report at the same minute, and the ERROR `<cache>
  watcher was not repaired` carries one `reason`, `no repair within 1m0s`: the other it could
  carry, `repair failed`, is gone, because a rewatch that failed is retried rather than reported.
- Dispatch's document settlement keeps the mark of an ask on the document its anchor names when
  the ask also names an ask block on another document; it reads asks by the anchor's document
  alone through a new index, `asks_anchor_artifact_id` (migration `0148`), so the read stays an
  index scan and the marks of every other ask are unchanged. An ask an earlier release stored
  with an anchor whose document id is not a uuid no longer fails the migration, the issue's ask
  list, its events, search or the references graph: it resolves to no document, and each boot
  logs how many such asks there are.

## [24.2.0] - 2026-10-30

### Added

- `envoy-dispatch` serves `GET /metrics`, unauthenticated beside `/healthz` and `/livez`, so
  whoever reaches Dispatch's HTTP port can read it, carrying `envoy_dispatch_outbox_publish_failures_total`: one count
  per destination of an outbox publish attempt that did not reach NATS, labelled `reason` over
  `denied` (the server refused the publish under the connection's own publish permissions),
  `refused` (NATS refuses the envelope or its subject however often it is retried) and `failed`
  (every other failure). The counter is registered whether or not the process has a bus, so the
  endpoint answers from the moment the server does and a process with no bus reports zero.
- The secrets broker reads four settings for Google VM identities: `BROKER_GOOGLE_OIDC_AUDIENCE`,
  the audience a VM's identity token must carry (its issuer is always Google's), and
  `BROKER_VM_SERVICES`, `BROKER_VM_PROJECTS` and `BROKER_VM_INSTANCES`, which bind one
  user-managed Google service account to one service name, one project and one instance-name glob.
  The broker refuses to start, naming the setting and the entry, on a malformed entry, an account
  given twice or with capital letters, a project's default account (Compute Engine's or App
  Engine's), a Google-managed service agent, a glob holding a character no instance name has, a
  VM account missing its project or glob, a project or glob for an account no VM service lists,
  any VM setting without the audience, or the audience without a VM service. With none of them
  set, it starts as before.
- `envoy-infra:gcp:Dispatch` takes `natsInCluster`: the app's `envoy-dispatch` and the
  listener's `envoy-listener` dial `natsUrl` themselves, at an address their own cluster
  resolves, so neither carries `ENVOY_NATS_SOCKS5_PROXY`, the front door's pods run no SOCKS5
  proxy, the `<hostname>-tailnet` Service on port 1055 and the ingress rule that admitted those
  two workloads to it are not rendered, and neither Deployment waits for the front door. A
  `natsUrl` that names a local server is then refused, naming `natsUrl`, since the pods would be
  dialling themselves. It also takes `natsReplicas`, the copies JetStream keeps of every stream
  and bucket (1 to 5, and not `replicas`, the app's pod count), and `streamMaxBytes`, the
  stream's byte limit, each rendered on both of those containers and independent of
  `natsInCluster`. With the three unset, the SOCKS5 path renders as it did before.

### Changed

- Dispatch's webhook redelivery sweep asks GitHub again for every failed delivery, whatever its
  status code (a 401, 400 or 422 included), until GitHub records an attempt answered OK or 72
  hours pass from the first failed attempt it saw, waiting 2, 4, 8, 16, 32 and then 60 minutes
  between requests. It logs `webhook delivery recovered` (with the request count) and `webhook
  delivery expired undelivered` (with the reason: the 72 hours, or GitHub answering 404). A first
  sweep with no saved position looks back 72 hours. A stored record marked terminal or exhausted
  by an earlier release is asked for again while inside the 72 hours. Dispatch answers
  `GET /api/v1/github/app-installation/{owner}/{repo}` with whether its GitHub App is installed on
  the repository. The listener publishes a push with a very long ref on a cut subject instead of
  answering 422, and logs a GitHub delivery that fails again as
  `redelivery <delivery>: <action> refused` (or `failed`), which the `webhook-refused` and
  `publish-failure` alarms do not count: they count a delivery's first failure in each listener
  task. The daemon's NATS grant adds `$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS`, which the
  daemon uses to read the age of each consumer's oldest waiting event.

### Fixed

- A session that subscribed to an issue's topic now hears what the agents on that issue do.
  Every published event's payload carries `quiet` (`events.Quiet`), whether the event carries
  nothing for a reader following its owner topic: a delivery receipt, an anchor refresh,
  settings or per-viewer bookkeeping, a targeted message its session already had, an ask edit
  that only moved a version, or a version nobody named. That is the half of `Notify` that does
  not depend on who acted, and it is what a reader skips on. Readers skipped on `notify`, which
  answers whether to wake the issue's route and claimant and is false for every event an agent
  wrote, so `envoy_subscribe notifications.dispatch.issue.<KEY>.>` delivered a person's
  activity and silently dropped every agent's. `Notify` itself is unchanged.
- A JetStream publish NATS denies is now named a denial rather than reported as an unreachable
  NATS, on both the windowed publishes Dispatch's event outbox makes and the synchronous ones
  every other caller makes, the Envoy listener's HTTP publishes among them. The server
  acknowledges such a publish with nothing, so the denial is read from the violation the server
  reports on the connection, recorded under the subject it names (`bus.ErrPublishDenied`), where
  the publish's own answer is a publish-window timeout or a no-responders error. Every refused
  message is named its own denial: the server refuses each message and reports a violation of
  each, while the connection keeps one last error, so one batch's several destinations - a
  notifying event's owner topic and its route - and its several messages to one subject - two
  comments on one issue a moment apart - used to leave every publish but one reading as an
  outage of the bus. An operator sees the difference on Dispatch's outbox: such a publish was
  released at INFO with no attempt counted, logged as `NATS is unreachable`, and the rest of
  that scan's backlog was abandoned; now the attempt is counted and backed off by its retry
  delay, and the ERROR line `dispatch outbox: publish event` names the subject and the server's
  permissions violation, so a missing grant reads as a missing grant and the events behind it
  still publish. On the Envoy listener, a `/v1/messages/send` or a `/v1/messages/publish` to a
  topic the stream captures that the listener's own grant denies is now the `403` with no
  `Retry-After`, recorded `outcome=publish_denied`, that a denied core publish already was,
  where it was a `503` with `Retry-After: 1` that a client retried until its attempts ran out.
- A document, a past version of it and a comment holding the same markdown read alike: no mark reaches an inline leaf (raw HTML, a picture, a hard break, a footnote reference), an inline HTML declaration (`<!` and any ASCII letter) is raw HTML to both readers and is stored unescaped, and an autolink inside a link label carries its own link while the label keeps its own on the runs around it, which makes such a document storable instead of refused. A line that begins, after up to three spaces, with `<!` and an ASCII letter now opens an HTML block, as the browser editor reads it, so a write holding one is refused (`block HTML is not accepted by Proof`): `<!doctype html> starts every page.` was stored escaped and is refused here.

## [24.1.0] - 2026-10-30

### Added

- Dispatch serves `GET /api/v1/agents/{session_id}/answers` (any caller): what people wrote that
  Dispatch delivered to one session since `since` (default 72 hours ago, at most 7 days), newest
  first, which every agent host will carry on each model request. It lists a person's
  `ask.answered`, `comment.created`, `message.created` and `message.answered` events whose
  `published_destinations` hold the session's own topic, its sent message deliveries (targeted and
  broadcast messages) and its sent comment deliveries (mentions), each item once at its current
  words: an ask's current answer (`replaced` when it changed an earlier one), a comment's current
  body (`edited_at` when edited), a message's body. Each carries its relation to the session, the
  ask and its question where it is in one, its owner, `dispatch://` ref and deep link. `max_chars`
  stops the list after the item that takes the running sum of text lengths past it, while `total`
  counts the whole window. The read is `sessionanswers.List`, which the outbox's tests read back
  through after publishing. Migrations `0145` (`events_person_words`, a partial index on a person's
  events of those four types by time), `0146` (`comment_deliveries_session`) and `0147`
  (`message_deliveries_session_created`, a partial covering index on a session's sent message
  deliveries by time) serve it; on 1,000,000 events `0145` built in about a second and the route's
  query ran in 6-8 ms. Through `0147` the read of a session's message deliveries, in a cached
  generic plan as well, is an index-only scan of only those since an hour before the window: on a
  session holding 60,003 deliveries, 1,019 of them in range, it took 0.3-0.4 ms and 11 buffers.

### Fixed

- An upload that moves an open anchor's quote where that anchor cannot stand, and leaves no copy
  of it where it can, is answered `400 INVALID_MARKDOWN`, naming the anchor, its quote and where
  the text landed, so the uploader can move the text or resolve the anchor first. That is an ask's
  quote moved into a code block, or the quote of any open ask, comment or suggestion moved into the
  front matter. Before, it was answered `500 DOC_SCHEMA` and logged as the server's error. An
  upload that copies anchored text where that anchor cannot stand (an ask's quote into a code
  block, any anchor's into the front matter) and keeps the original is taken, and the anchor stays
  on the original; before, it was answered 500 too. A comment's or suggestion's quote in a code
  block is still re-anchored there, as before.
- A document upload keeps the block ids of the untyped blocks it leaves unchanged, and writes the
  open anchors on text it keeps without stripping and adding them again. A block reference or
  pinned block link a person made into a document now keeps resolving after an upload that edits,
  adds or removes other blocks and leaves its block alone; before, every upload gave every block a
  new id, so the reference pointed at nothing. An upload that moves a block still gives new ids to
  that block and to the blocks it moves past. The same re-minting, with every open anchor
  re-marked, made a live room keep about half a megabyte more for each upload of a 25 KB spec; a
  one-token upload now adds about 2 KB, near what it stores.

## [24.0.0] - 2026-10-30

### Added

- Dispatch splits every comment and message into blocks that a reference can name (`dispatch://KEY/comment/<id>#b-<block>[@v<N>]`, `dispatch://KEY/message/<id>#b-<block>`); a link in a comment or issue message starts at its block, a reply names the block it answers with `reply_to_block` or `in_reply_to_block` and carries `reply_block` on every read and event, `GET /api/v1/comments/{id}/blocks` and `GET /api/v1/messages/{id}/blocks` list an item's blocks, and a comment edit keeps each block's id. A comment edit's `references_changed` names a target whose link moved to another block of the comment, and an open dashboard page shows a comment block's new text after its comment is edited, and a new reply under a block it names, with no reload.

### Changed

- **Breaking:** `envoy-infra:gcp:Dispatch` takes `clusterOnly`, which builds the cluster alone:
  the VPC, its subnetwork, the router, Cloud NAT and the Autopilot cluster, and nothing else. It
  registers no private services access range or peering, no Cloud SQL instance, database or user,
  no generated password, no Secret Manager grant, no Kubernetes provider and no Kubernetes
  object, so it takes a `gcp` provider alone; only `region`, `hostname` and `deletionProtection`
  come with it, and every other input is refused by name. Thirteen inputs that configure Dispatch
  itself are now optional in the schema — `tailnetDnsName`, `tags`, `tailnetAuthKey`,
  `tailnetService`, `envoyImage`, `tailscaleImage`, `cloudSqlProxyImage`, `frontDoorToolsImage`,
  `natsUrl`, `natsSeedSecret`, `signInClientId`, `signInClientSecret` and `signInDomain` — and the
  resolver is what requires each of them while `clusterOnly` is off, refusing a missing one by
  name as it always did. A regenerated Go SDK therefore changes seven plain inputs from `string`
  to `*string`: `tailnetDnsName`, `envoyImage`, `tailscaleImage`, `cloudSqlProxyImage`,
  `frontDoorToolsImage`, `signInClientId` and `signInDomain` take `pulumi.StringRef(…)` where
  they took a string. The five `pulumi.*Input` ones, `tailnetAuthKey`, `tailnetService`,
  `natsUrl`, `natsSeedSecret` and `signInClientSecret`, change from `pulumi.StringInput` to
  `pulumi.StringPtrInput`: a `pulumi.String` or a `pulumi.StringOutput` still compiles there,
  and a value whose static type is `pulumi.StringInput` does not. `tags` does not change. A
  stack that sets nothing new sends nothing new: the input has no schema default, and the
  component's declarations with `clusterOnly` off do not move.

### Fixed

- A link or hover card to a block on an open Dispatch page stops showing the block's text once an edit deletes the block, for a block of a document as for a block of a comment or message, instead of keeping the old text until a reload.
- Two Envoy programs that start at the same moment against one clustered NATS both start, and so
  do two keeper passes that meet. Every nats-server before v2.12.5 (every v2.10 and v2.11 release
  included) can refuse the later of two updates of one stream with `nats: subjects overlap with an
  existing stream`: the server checks the new configuration against the account's streams while
  skipping only the assignment it read, so an update that lands in between leaves the stream
  overlapping itself. A listener's start then stopped with that error, behind
  `ENVOY_NATS_REPLICAS=<n>: ` when it raised the copies and `raise KV_<bucket>: ` when it raised a
  bucket's, Dispatch's server logged `envoy nats reconnect hook failed`, and a keeper logged that
  it could not restore the stream, though nothing else overlapped it. Such an update is now
  settled from the stream, as one whose answer never came already was: it succeeds when the
  stream, read again, holds what the program asked for, and is sent again when it does not, up to
  five sends. A stream another stream really does overlap is still refused, after the fifth send,
  and a refusal of a setting the deployment must change, such as more copies than the cluster has
  servers, still fails the start and still names that setting.

## [23.1.0] - 2026-10-29

### Added

- `envoy-infra:gcp:ClusteredNatsHub` runs NATS with JetStream as three nats-server pods on a GKE
  cluster the consumer already has, one per zone, through the NATS project's `nats` Helm chart
  2.15.1, on a Kubernetes provider it builds from the cluster's `kubeconfig` with mutable
  ConfigMaps. It renders the same users as `envoy-infra:gcp:NatsHub`, through the same function, so
  a hub moves from the VM with its users, and with its listener's durable when the listener's
  machine id stays the VM's hostname. A users change rewrites the chart's `auth.conf` in place and
  the chart's config reloader applies it with no server restart and no client reconnect; a
  session-user change or a change of the file store's limit (nats-server refuses to reload a new
  `no_auth_user` or a lower `max_file_store`) restarts the pods one at a time, each on the new
  configuration. The chart's StatefulSet is protected, so a change that would replace it, a disk
  size or a storage class among them, fails the preview rather than stopping all three servers; the
  module's README gives the steps that grow the disks and the step before a teardown. A
  NetworkPolicy admits the client port from the namespaces the consumer names, the route port,
  whose routes carry no credential, from the hub pods alone, and the exporter's port from Google
  Managed Prometheus, which a PodMonitoring points at each pod's exporter. With `tailnet`, a
  Deployment of at least two tailnet hosts serves the hub as a Tailscale Service on TCP 4222 and
  9020, each host running Envoy's session listener on its loopback and advertising the Service only
  while that listener answers `/healthz` with 200. Every host runs the listener as one machine id,
  so `tailnet` needs a listener build with a single-deliverer take-over (one pod delivers from the
  durable while the others wait as standbys), which no release has yet: with an earlier build a
  second host's listener exits every 135 s while it waits for the durable, being advertised in the
  meantime, and two hosts can deliver one message twice after a NATS reconnect. `streamMaxBytes`
  sizes JetStream's file store, each pod's disk and nats-server's memory from the measured sizes of
  the listener's notification stream, and nats-server runs with `GOMEMLIMIT` at 80% of its memory.
  The component refuses each invalid input by name before it registers anything, as
  `packages/envoy/infra/README.md` lists.
- `envoy-infra`'s `core.ListenerConfig` takes `NATSReplicas` and `StreamMaxBytes`, rendered as
  `ENVOY_NATS_REPLICAS` and `ENVOY_STREAM_MAX_BYTES` after every other variable, each only when it
  is above 0, so every rendering made before them (`host:Listener`'s, `gcp:NatsHub`'s cloud-config,
  `gcp:Dispatch`'s containers) stays byte for byte. `Check` refuses either below 0, and a
  `NATSReplicas` above 5, the most copies NATS keeps of a stream.
- GET /api/v1/issues/{key}/references takes `versions=latest`: each member's document then carries only its latest version, under an ETag of its own (If-None-Match still answers 304). With the parameter absent, empty or `all`, the answer, its headers and its ETag are unchanged; any other value, or `versions` given more than once, is `400 INVALID_QUERY`. The answer is one statement that joins each document to its highest-numbered version.
- The load test's `stack.sh seed` adds production-shaped `dispatch://` references (`loadgen seed-refs`): 1,000 cited documents with 19,471 versions, drawn from the committed per-member version counts of 160 production references answers (`refs-sample-2026-10-27.json`, no keys), and one `references.md` on each of 310 LGA issues, so a step's `GET /api/v1/issues/{key}/references` does the work it does in production. `loadgen refs-shape` checks the answers against production's shape and `loadgen refs-fetch` records each one; `seed.meta.json` counts the references. The seed takes about 23 minutes more on 2 vCPU.
- Dispatch's new-issue dialog lists what it already holds on the same subject while a person
  types the title. A title that has rested 400 ms is checked against the project through
  `GET /api/v1/projects/{key}/related`, which records nothing, and up to three related items —
  issues, asks, comments, messages and documents — are listed under the Spec field, each named
  by its owner and linked in a new tab so the draft survives the click, an ask carrying where
  it stands and its latest answer, with the past decision that may already answer the question
  named under them. Typing sends one check per pause rather than one per keystroke, a check a
  newer title supersedes is aborted, and an answer that arrives after the title changed is never
  shown under the new title. A check that searched and found nothing and a check that could not
  run read differently, the second naming the words to search Dispatch for, and a failed check
  says so with Retry; nothing is asked below two characters of title or with no project chosen.

### Fixed

- A Dispatch document written through the API that the server reads as the browser editor reads it is now stored, where the server refused some with `400 INVALID_MARKDOWN` because the markdown it would store read back otherwise. The shapes it now stores include a letter right after a backslash beside an emphasis run (`p \~\a*~* q`, refused since sjawhar/legion#2360, now written with the backslash doubled before the letter's character reference), a bare web address before a delimiter run (`*http\://a.example/a*b\)**`), an asterisk run beside bold or italic code, emphasis whose marks end apart (`***![\![`c`**\**`), and a code block whose language holds a backtick, which now takes a tilde fence. A document stored correctly before keeps its bytes: the new spellings are tried only where nothing else reads back.
- Dispatch's subscribers list shows a subscriber whose session is no longer live with its session's
  last heartbeat again, not its interest row's `updated_at`, which can be up to 5 minutes earlier
  since the listener rewrites an unchanged row only that often (sjawhar/legion#2377). The
  listener's `GET /v1/interests/` and `GET /v1/interests/<session_id>` rows now carry `last_seen`,
  the session's last heartbeat as its session cache knows it, omitted when unknown, and the
  listener keeps a dead session's last heartbeat until the interest reaper's grace has passed
  (`store.InterestReapGrace` + `store.InterestRefreshAge` after the session left the cache,
  `session.WithLastSeenRetention`), so every row the reaper still holds can carry it. Dispatch
  falls back to the row's `updated_at` when the listener sends no `last_seen` (it restarted since
  the session died, or predates the field).
- A listener's NATS reconnect, and a self-health rebuild of a cache watcher, no longer read the
  interest, role claim, session and CI buckets again in full. Each `Rewatch` now resumes after the
  last revision its cache applied and delivers only what was written since, and its
  `<cache> cache warm-up` line says `outcome: "resumed"`, with `entries` and `delete_markers`
  counting what was written since. A listener start still reads every bucket in full, and so does
  a rewatch onto a recreated bucket, after a scan that did not complete, or onto a stream whose
  last sequence is below the last revision applied, and so does the role claim watcher's rescan of
  a scan that timed out. A resume whose own watch cannot be created falls back to the full read in
  the same rewatch, so a slow server costs the full path rather than a failed repair. A resumed
  watch makes its cache complete (`kvwatch.Watcher.Complete`) at its own end-of-scan marker, as a
  full scan does. A resume sees what a full read of the same bucket saw: a delete marker purged,
  or a value `MaxAge` expired, while the watcher was away leaves nothing to deliver either way.
- Session-cache reads no longer queue behind one another, and an interest reaper pass no longer
  holds up subscribes. Every read of a listener's session cache (`GET /v1/sessions`, a delivery's
  liveness check, the `envoy_active_sessions` gauge) took the cache's exclusive lock and walked
  every cached session to remove the expired ones, and the interest reaper checked each session's
  liveness while holding the interest registry's lock, so each subscribe waited out the whole
  pass. Reads now take a shared lock and change nothing; an expired session leaves the cache at
  its deadline, on a timer, and `last_seen` answers the same before and after. The reaper checks
  liveness with no interest lock held, re-checks each candidate and deletes it only at the
  revision it checked, so an interest written during the pass is kept. Each `reaper cycle` line
  now carries `elapsed_ms`.
- A heartbeat that adds no topic no longer rewrites the session's unchanged interest row:
  `POST /v1/interests/subscribe` writes it once the row is 5 minutes old
  (`store.InterestRefreshAge`), and until then answers with the stored row, `updated_at`
  included. A changed row is still written at once. KV writes per 90-second heartbeat fall from
  two to about one and a quarter. The session row is still written on every heartbeat, since its
  bucket's `MaxAge` is the liveness clock. Run this build only once every listener on the
  `envoy_interests` bucket reaps at the grace window plus `store.InterestRefreshAge` (the release
  of sjawhar/legion#2374): an older listener reaps at 10 minutes after the row's last write, and
  could delete a paused session's interests 5 minutes after its last heartbeat.

## [23.0.0] - 2026-10-29

### Changed

- Editing an ask through the ask-edit route (`PATCH /api/v1/asks/{id}`, `dispatch edit-ask`) now
  refuses, with HTTP 400 `ASK_BLOCK_TEXT`, every edit of a decision that breaks its content rule
  (a second option list, or a block after its options, which two people's concurrent edits can
  leave), so the route never moves or drops what another person wrote. The reason quotes every
  block of the decision in full with its block id as quoted data, and names the path: delete the
  blocks it lists, all in one document edit (`dispatch doc-edit`), which keeps the question's
  paragraphs and the first list after them and does the same inside any decision nested in those
  blocks, so one document edit repairs them together, then edit the ask; where one edit cannot
  (a decision holding this one is broken too, or a decision needs words written rather than
  blocks deleted), it names no delete and says the decision is repaired in the document
  editor; before the document has settled its block ids it says to
  retry instead. Before, an edit of only its urgency or
  `multiple` wrote its options, joined across both lists, to the ask.
- **Breaking:** migration 0141 drops `broadcast_idempotency_keys`, the broadcast send's own key
  table from before every write took an `Idempotency-Key` header (INFRA-258). No value anything
  reads is lost: since 0137 a broadcast's key is a `request_keys` row, and the send route no
  longer takes the body key the dropped rows recorded. Rollback floor: once 0141 has applied, a
  task older than legion-envoy v21.0.0, the release that added 0137, still boots, but its
  broadcast send fails with SQLSTATE 42P01 (undefined table), since it writes
  `broadcast_idempotency_keys`. v21.0.0 and every later release neither read nor write it. The
  drop takes `ACCESS EXCLUSIVE` on `broadcasts` for the instant its transaction lasts, so a
  broadcast read or send waits that instant.

### Fixed

- An ask that two people's concurrent edits leave holding two option lists, or a paragraph after
  its options, now keeps every word in every editor and in the stored document, stays open or
  decided as it was, and shows as a malformed decision until its text is fixed. The browser editor
  refused to build such an ask, so y-prosemirror deleted it, with all its text, from the shared
  document every editor and the server read, and settlement then retracted it. The editor now builds
  an ask holding any non-empty run of the blocks the server accepts in one, and inside such an ask
  an input whose edit it cannot make, such as Enter twice to leave a list, Backspace joining a list
  item into the question, typing over a selection, cutting or dropping, is declined instead of
  being applied by the browser; an input method that starts composing over a selection across
  blocks types at the selection's start without deleting it; and an edit the browser makes itself,
  such as a writing extension's `document.execCommand`, is refused when it would delete any of an
  option list or paragraph the person did not select. Its own edits,
  input rules, pastes, markdown imports and retyping a block as an ask still hold an ask to
  `paragraph+ bullet_list?`.
- Settlement now reads an ask that parses but breaks its content rule, a second bullet list or a
  paragraph after its list, as malformed: it marks the block `invalid` with the content rule's
  reason (`pmdoc.AskContentError`) and emits `block.invalid`. An open ask's row stays open and
  unchanged, as it already did for an ask holding a heading; an answered or resolved ask keeps its
  decided row and answer. An edit that makes the ask conform clears `invalid`, updates the row, and
  writes a decided ask's decided wording back into it. It used to index such an ask as valid, with
  its option lists concatenated and its question's paragraphs joined. This changes how such a
  document is read: at the time of the change, a census of every live document in all 11 Dispatch
  projects (6,029 documents, 636 ask blocks, 19 of them open) found no ask it newly marks.
- The document editor now draws a pasted container directive it refuses, an unknown typed block
  such as `:::note` or a typed block with a refused attribute, as its literal lines around its
  formatted content, and so does a past version's view that holds one. It threw, so a paste fell
  back to the browser's own plain-text paste, with the lines joined. A past version's view also
  keeps an answered decision's answer. The editor also no longer refuses the paragraph-line
  directive forms: a line opening `::name` (a leaf directive), a `:name{…}` text directive, and a
  malformed `:::` line such as a Pandoc `::: warning` fence now stay the text they are, which the
  server escapes when it saves them. Only those inputs read differently: a pasted ask with two
  option lists or a paragraph after its list is still drawn as its lines, and the headless
  engine's default mode, which the server's parity fixtures are generated with, still refuses
  all of them.
- On `/ws/live`, a subscription that replaces a delivery view under the same sub no longer has its
  first frame written ahead of the hub's answer to the view it replaced. The socket's writer let the
  stream in when its look found the hub's queue empty, and a hub frame that arrived before its pick
  then went out in either order, so a page could read the replaced view's answer, such as
  `DELIVERY_NOT_CONFIGURED`, after the events or agent subscription that took its place had begun.
  A stream frame the pick takes while a hub frame waits is now written once the hub's queue is empty.

## [22.3.1] - 2026-10-29

### Fixed

- Indented code keeps the whitespace-only lines after it that the browser keeps (four or more
  columns past its container, and the shorter blank lines between them), and those lines no longer
  spread a list or a list item, so a document reads in the editor as in a comment and a past
  version. A whitespace-only line in fenced code whose fence is indented, holding fewer columns
  than the fence, is an empty line of the code, as the browser reads it. A document's last line
  with no line feed reads as the browser reads it too: it keeps the columns its containers leave of
  a tab, a typed block indented in its container takes its indentation from it, and in fenced code
  one holding nothing past the fence's indentation is no line of the code. A stored document is
  read anew at its next read.
- A `bus.ConnectOwningStream` whose stream reconcile the server refuses (more copies than it has
  servers, a byte limit it cannot reserve) leaves no connection open. The constructor closed the
  connection it had dialled, but that close started a recovery of the client it never returned,
  which dialled and installed a replacement no caller could close; once its server went, that
  connection reconnected for the rest of the process and logged `envoy nats reconnect attempt
  failed` once a minute. The client is now stopped before the close, so the recovery dials nothing.
- A Dispatch suggestion comment no longer reads its whole document again to learn the suggestion's
  kind: a read inside a write transaction (`SuggestionKind`, `BlockForQuote`, `VerifyMark`) uses the
  rendering the transaction's last write already made of its fork, as the version capture and the
  margin write do, through one rule (`liveWrite.rendering`). On real-size specs this makes the
  kind read about 80% faster and the whole suggestion comment about 10% faster. A document write
  whose change panics now always drops its transaction's fork, so a later write in the same
  transaction can no longer build on a fork no recorded update describes and write a version the
  committed document does not hold.

## [22.3.0] - 2026-10-29

### Changed

- `POST /v1/messages/publish` answers a role or ad-hoc topic message once its recipient confirmed
  it, or 504, so a message sent through a listener reaches its recipient or its sender gets an
  error, also while the hub's NATS servers change under the recipient. A role topic is
  no longer published on the role lane: the listener forwards the envelope to the live holder's
  agent subject itself with a receipt, resolving the holder again and forwarding the same envelope
  (one `event_id`, dedupe key `envoy.role.forward.<key>`) 100 ms after each miss, each forward
  waiting at most 2 s, until a receipt arrives or 4 s from the request's arrival have passed. It
  answers `200` with the envelope and `holder` once the holder's receipt arrived, and also,
  forwarding nothing, when `source_session` holds the role or the holder already confirmed the
  dedupe key through the route; `404` with `reason` `unclaimed` or `holder_lapsed`, as before,
  and for a first resolution of the holder that fails the answer `GET /v1/roles/<role>` gives (the
  503 with `Retry-After: 1` for a read that did not answer within 3.5 s of the request's arrival
  or for a lost race for the claim, the 500 for a claim record no read can decode);
  `403 {"error":…,"reason":"capability","holder":…}` when the holder does not advertise the
  payload's delivery mode, where the role lane dropped such a message after the 200; and
  `504 {"error":…,"reason":…,"event_id":…,"dedupe_key":…,"holder":…,"attempts":N}` once the 4 s
  passed, `reason` being `receipt_timeout` (the last forward reached the server and drew no
  receipt) or `delivery_failed` (no live holder, a read that failed or ran out of time, a forward
  not shown to have reached the server). An ad-hoc topic (one no stream subject captures,
  `bus.UsesCoreTransport`) is still published over core NATS, then forwarded the same way,
  `event_id` and `dedupe_key` unchanged, to the agent subject of each live `self_subscribed`
  session registered on it on any machine
  (`store.Registry.MatchAll`), the sender excepted; it answers `200` once it was published and
  each confirmed or there was none. Its own publish not reaching NATS, the 4 s ending first or the
  publish failing at once for any reason but a refusal, answers `504 {"error":"<topic> was not
  published within 4s: <cause>; no session was sent it","reason":"publish_failed","event_id":…,
  "dedupe_key":…}` and forwards to no one; a refusal keeps its 400 or 413, and a publish the
  listener's grant denies its 403. A topic published
  that some recipient did not confirm answers `504 {"error":…,"reason":"not_confirmed",
  "event_id":…,"dedupe_key":…,"delivered":[…],"undelivered":[{"session_id":…,"reason":…}]}`. Only
  a receipt that arrived through the route counts as a recipient's confirmation: the route keeps a
  record of its own of who confirmed each dedupe key, which a re-send of the key reads, since the
  live topics lane's fanout records a self-subscribed session whose push it skipped in the
  delivery handlers' dedupe cache without hearing from it. A session that stopped without
  unregistering stays registered for up to 5 minutes and is named in `undelivered` in that time; a
  named session is not a failed delivery to the others. A session served on a port is not
  confirmed and keeps the live topics lane's delivery. A topic the stream captures is unchanged:
  stored before the 200, and read back through `POST /v1/replay` by a session that missed it. The
  route publishes no delivery exception. The role lane now carries only a role topic a client
  publishes straight onto NATS (a listener of an earlier release, an operator's `nats pub`), and
  still forwards it once and reports a failure as an exception. Every answer is written with the
  route's one `listener publish` record line, its `outcome` `accepted`, `role_unclaimed`,
  `role_lapsed`, `capability_refused`, `receipt_timeout`, `delivery_failed`, `not_confirmed`,
  `publish_failed` or a refusal's own, beside the send's own `listener publish role send` or
  `listener publish topic send` line.
- Every receipt-backed forward to a session's agent subject (`bus.Client.RequestCoreTo`) carries a
  `Nats-Msg-Id` of its dedupe key and subject (`bus.ForwardMsgID`), so the stream, which captures
  agent subjects, keeps one copy per message and recipient however many attempts the route makes;
  `RequestCoreTo` takes as a receipt only a reply with no data and no header field, since the
  stream's acknowledgement of that copy carries data and the server's no-responders answer a
  `Status` header. A listener's durable acknowledges an envelope stored on an agent subject whose
  topic is not that subject (a stored forward copy) without delivering it. During a rollout a
  listener of an earlier release sends a stored ad-hoc forward copy to its fanout, which pushes it
  to its serve-port sessions subscribed to the topic once per key. A soak test
  (`cmd/listener/server_change_test.go`, build tag `serverchange`) runs both listener binaries
  over a three-server in-process NATS cluster and proves every role, ad-hoc and captured message
  sent every 250 ms arrives or errs across lame-duck and killed servers; CI runs it as its own
  job, `listener-server-change`.
- The Legion daemon reads the new answers (`packages/daemon/internal/notify`,
  `internal/daemon/outbox.go`; the daemon keeps no CHANGELOG of its own). A 504 `receipt_timeout`
  queues the notice again on the existing capped schedule, at most three copies; a 504
  `delivery_failed` is retried on the row's backoff without a cap, so a hub outage drops no notice;
  a 403 `capability`, and the 403 whose error opens with the listener's grant-denial prefix
  (`notify.ErrPublishDenied`), each finish the row undelivered with one log line naming the reason,
  so no later notice waits behind it, and for a merge-queue packet post a Dispatch message naming
  the refusal. It still re-holds from the role-lane exceptions a listener of an
  earlier release raises. **Release order:** a deployment's Legion daemons take this release
  before its listeners do. An earlier daemon reads both new answers, the 504 and the 403
  `capability`, as a generic error and retries the notice without a cap, and every later notice to
  that architect waits behind it (`earlierNoticeFor`): behind a 504 until the holder confirms or its
  registration lapses, behind a 403 until another session holds the role. Neither answer refuses
  input an earlier listener accepted: each reports a message that an earlier listener answered
  200 and then failed to deliver (a forward without a receipt, or a holder that does not advertise
  the payload's delivery mode). A role held by a session served on a port, which holds no NATS
  connection to confirm a forward, now answers 504 `receipt_timeout`; an earlier listener answered
  200 and the forward reached nobody.

## [22.2.1] - 2026-10-29

### Changed

- A Dispatch document read that misses the cache validates the document tree once instead of five times, and reads each text from its Yjs items directly instead of through ygo's `ToDelta` (`pmdoc.ReadUnshared`, `pmdoc.ValidDocument`). A live `/blocks` read builds its blocks from the tree it read with one validation instead of four. Tokens, `/text`, `/blocks` and every refusal are byte for byte the same. The token encoder `pmdoc.AppendTokenJSON` is now `pmdoc.EncodeTokenJSON`, which allocates its own buffer.

## [22.2.0] - 2026-10-29

### Added

- Dispatch tells whoever files an issue, an ask or a decision block what it already holds on the
  same subject. After an issue create, an ask (on an issue or a project document) or a
  document edit that adds decision blocks commits, the related-item check searches the project by
  the text's rarest words (at most 25, each scored by its count in the text times its BM25 idf
  over the project's items, with too-common words, numbers and hashes dropped, and the keyword
  arms ranked by idf-weighted coordination of those words) and, where an embedder is configured,
  by meaning, and the response waits for it at most 5 s. `advice.suggestions` now always says what
  it did: `outcome` `found`, `none` or `not_made` (with the reason in `missing`), `search`
  `meaning_and_keywords` or `keywords_only` (with `search_note` saying why), `search_query` for a
  reader's own `dispatch search`, `related` (each ask item with `ask`, its state, latest answer,
  who gave it, when and the options chosen; no item carries `ask`, and there is no `decision`,
  when the asks' states could not be read) and `decision`; a forced issue create also lists the
  titles it forced past in `title_matches`. A document edit's advice gains
  `decision_block_checks`, `{block_id, question, suggestions}` for each block it added in
  document order, the first five checked together under the one 5 s bound and the rest answered
  as not made. `GET /api/v1/projects/{key}/related` (title and text at most 1,000 characters
  together) and `POST` (a spec-length text) run the same check and write nothing.
  `GET /api/v1/related-checks?project=&source_kind=&from=&to=` counts the recorded checks by
  source, outcome, search mode and reason, with p50, p95, maximum and over-300 ms counts of their
  keyword and total times. Migration `0139_related_checks` adds `related_checks` (one row per check
  of a write) and `search_term_stats` / `search_term_totals`, the word statistics the new
  term-statistics refresh (`api.RunSearchTermStatsRefresh`, lease `search-term-stats`, so the
  lease pool holds 13 connections) rebuilds for every project at boot and every 10 minutes.
  Migration `0140_issue_embedding_spec` makes an issue's meaning-search text its title and its
  spec, re-queued on a spec edit; the coverage pass re-embeds existing issues once an embedder
  runs. Both censuses answer `0`.

### Changed

- A write's related items no longer come from LEGION-550's 300 ms keyword step
  (`writeSuggestionTimeout`, removed): an issue create, an ask or a document edit adding decision
  blocks can now take up to 5 s longer, the related-item check's bound, and a check that runs out
  of time says it was not made instead of looking like one that found nothing. The
  route descriptions of `POST /api/v1/issues`, `POST /api/v1/issues/{key}/asks` and
  `POST /api/v1/artifacts/{id}/asks` say so.
  A Dispatch server process now opens up to 87 database connections, one more for the
  term-statistics refresh's lease, and envoy-infra's `gcp:Dispatch` sizes an app pod by that count.

## [22.1.0] - 2026-10-29

### Changed

- `ENVOY_NOTIFICATIONS` and every key-value bucket Envoy opens (`envoy_interests`, `envoy_roles`,
  `envoy_sessions`, `envoy_ci_state`, `envoy_webhook_redelivery`) now hold the deployment's
  replica count, `ENVOY_NATS_REPLICAS`: a whole number from 1 to 5, default 1. A start creates the
  stream or a bucket at that count and raises one it finds with fewer copies; the keeper (below)
  raises it again after a concurrent start lowered it. The copies stay at the setting only when
  every program of a deployment carries the same value: a clustered deployment sets it in every
  envoy-listener and in the Dispatch server. Lowering copies is an operator step that needs every
  program's setting lowered first, or the keeper raises it back
  (`nats stream edit <stream> --replicas=<n>`). The listener reads it in `config.Load`, and now
  refuses 0, a negative number, more than 5 and a non-integer at load, naming the variable, where
  it took any integer. Dispatch's server reads it from a new settings table row, where it created
  the stream and its redelivery bucket with a single replica, and so does
  `envoy-dispatch redeliver-webhooks`. More than 1 needs a clustered NATS: a single server
  (nats-server v2.15.1 included) refuses both a create and a raise above 1, and the listener's start
  then stops with an error naming `ENVOY_NATS_REPLICAS=<n>` (Dispatch's server, which connects in
  the background, serves on and logs `envoy nats reconnect hook failed` with that error); a refused bucket raise names the bucket
  too, as `raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3: context deadline exceeded` (a grant
  without `$JS.API.STREAM.UPDATE.KV_<bucket>` shows as that timeout after 10 seconds). A cluster
  with one of three servers stopped creates a new stream at 3 copies, one offline peer assigned,
  and the start succeeds on nats-server v2.15.1; v2.10.29 refuses that create.
- `ENVOY_STREAM_MAX_BYTES`, new, read by the listener and by Dispatch's server (a new settings
  table row): a positive whole number of bytes, the stream's byte limit. A full stream refuses a
  new publish (`DiscardNew`, now set on every start and switching a deployed `DiscardOld` stream)
  and keeps every stored message, which leaves only by age, so the stream still holds every
  message it took for its whole 72-hour duplicate window and the limit drops none; size the limit
  above the stream's 72-hour volume. The refused publish's sender gets
  `bus.ErrStreamFull` (`stream ENVOY_NOTIFICATIONS is full: nats: maximum bytes exceeded`, the
  server's error 10077 kept inside), which is not a `bus.ErrRefused` and so is retried: the webhook
  listener answers GitHub 503 and Dispatch's redelivery sweep has GitHub redeliver it, Dispatch's
  outbox retries the event on its backoff, the CI store settles again on a later tick, and
  `/v1/messages/send` and `/v1/messages/publish` answer `503` with `Retry-After: 1` and the body
  `envoy: the message bus refused the message because its stream is full: …`, their request record's
  outcome `stream_full` rather than `bus_unavailable`, since the bus answered. Unset, the stream is
  created with no byte limit and a start keeps a deployed limit. Neither a start nor the keeper
  (below) ever lowers a deployed limit, as for the copies: each raises a smaller limit, or none, to
  its setting and keeps a larger one, so an operator's raise of a full stream
  (`nats stream edit ENVOY_NOTIFICATIONS --max-bytes=<n>`) stands, and programs with different
  limits do not fight over the stream: the largest stands. A concurrent start can still put back
  the smaller limit it read over another's (below) until that program's keeper raises it again.
  Lowering or removing a limit is an operator step that needs every program's setting lowered or
  removed first (`--max-bytes=<n>`, or `--max-bytes=-1` for none). 0, a negative number or a
  non-integer is refused at load naming the variable, and a limit above what the server can
  reserve is refused at create or update: the listener's start then stops with an error naming
  `ENVOY_STREAM_MAX_BYTES=<n>`, and Dispatch's server, which connects in the background, serves on
  and logs `envoy nats reconnect hook failed` with that error at each retry.
- A JetStream publish the server acknowledges as a duplicate with no sequence (`bus.ErrNotStored`,
  `stream ENVOY_NOTIFICATIONS acknowledged a duplicate of a message it does not hold`) is now sent
  once more without its `Nats-Msg-Id`, through `Publish`, `PublishReportingDuplicate` and a
  window's send, where it was taken as published. A clustered nats-server (v2.10.29 among them)
  records a publish's `Nats-Msg-Id` at sequence 0 before it stores the message and keeps that
  record on the stream's leader for the 72-hour duplicate window, a purge notwithstanding, when a
  full stream refuses it, so every retry under the id (a GitHub redelivery, a Dispatch outbox
  retry) was acknowledged `{"seq":0,"duplicate":true}` though the stream never stored the message.
  The send without the id is stored as soon as the stream has room, with no leader step-down, and
  the publish succeeds; while the stream is still full it is refused too, and the publish fails
  with an error wrapping `bus.ErrNotStored` and the send's own error (`stream ENVOY_NOTIFICATIONS
  acknowledged a duplicate of a message it does not hold, and its publish without its id failed:
  stream ENVOY_NOTIFICATIONS is full: nats: maximum bytes exceeded`), which is not a
  `bus.ErrRefused` and is retried as `bus.ErrStreamFull` is: the webhook listener answers 503, the
  outbox backs off, the send routes answer the full stream's `503` when the second publish was
  refused as full. The stream drops no repeat of a message sent
  without its id: a retry of a message still being stored, also acknowledged a duplicate with no
  sequence, stores a second copy, and once a refused message is stored, every later publish under
  its id stores another while the leader keeps the record, so the stream's consumers see each copy.
- Every update of `ENVOY_NOTIFICATIONS` a start sends now starts from the deployed configuration
  and changes only what Envoy owns: the subjects (reconciled as before), the 72-hour age and
  duplicate window, the replicas (the deployed count or the setting, whichever is more), the byte
  limit (the deployed one or the setting, whichever is more; the setting on a stream with none, the
  deployed one when unset) and `DiscardNew`. Every other deployed field
  (`MaxMsgsPerSubject`, `MaxMsgSize`, placement, compression and the rest) is kept beside the
  metadata, `envoy_stream_id` included, where an update that changed subjects, age or window wrote
  this binary's configuration over them, its replica count scaling a replicated stream down to the
  starting program's count, which for Dispatch's server was always 1. A start that finds the
  stream as it should be sends no update.
- The listener and Dispatch's server, the programs that own `ENVOY_NOTIFICATIONS`, now keep it
  while they run. JetStream's stream update has no compare-and-swap, so a start with other or no
  settings, or another subject list, that reads the stream before another start's write and writes
  after it puts the old subjects, copies, byte limit and metadata back. Every 30 seconds each
  program's keeper reads the stream again and puts back copies below its `ENVOY_NATS_REPLICAS`, a
  byte limit below its `ENVOY_STREAM_MAX_BYTES` or none (when set), a missing `envoy_stream_id`, the
  72-hour age and duplicate window and `DiscardNew` (compiled into every release alike, logged as
  `discard DiscardOld -> DiscardNew` when it puts it back), and adds
  back each subject its binary compiles that the stream lacks, unless it overlaps a subject the
  stream carries or captures a role lane, logging the WARN line
  `envoy nats keeper restored the stream` with, for example,
  `restored="replicas 1 -> 3, max_bytes -1 -> 2000000, envoy_stream_id added as <id>, subjects added notifications.legion.>"`.
  It only adds subjects: it removes and replaces none, so two releases with different subject
  lists do not switch the stream back and forth, and each release's keeper adds back its own
  subject that a concurrent write dropped. A stream that still carries role-lane subjects it
  leaves to a start, which moves them off: when that stream needs anything, it sends nothing and
  logs that it could not restore it. It also raises
  the buckets its program opens (the listener's `envoy_interests`, `envoy_roles`,
  `envoy_sessions` and `envoy_ci_state`, Dispatch's `envoy_webhook_redelivery`) back to
  `ENVOY_NATS_REPLICAS`, logging `envoy nats keeper raised a bucket` with `replicas="1 -> 3"`. A
  failed pass logs `envoy nats keeper could not restore the stream` or
  `envoy nats keeper could not raise a bucket` and the next period tries again; it never stops
  the program. With mixed settings during a rollout the stream can sit below the setting for up
  to one period after the last start.
- envoy-infra: `ListenerPermissions`, and through it `HostPermissions` and `DispatchPermissions`,
  gains `$JS.API.STREAM.UPDATE.KV_<bucket>` for each bucket the listener opens, and
  `DispatchPermissions` the same for the webhook redelivery bucket: the update a start, and the
  keeper every 30 seconds after it, sends to raise a bucket's copies. `gcp:NatsHub` renders it
  into the users it derives from those sets, so moving a hub with a `listener`, a `machineId` user
  or a `dispatchMachineId` user to this release replaces its VM once. A hub must hold the new
  grant (`gcp:NatsHub` moved to this release, or the subject added to a hand-written grant such as
  an `aws:NatsService` `authorization`) before a program with `ENVOY_NATS_REPLICAS` above a
  bucket's copies starts, or that raise times out after 10 seconds and the start stops naming the
  bucket and the setting: `raise KV_envoy_interests: ENVOY_NATS_REPLICAS=3: context deadline
  exceeded`.
- envoy-infra: `aws:WebhookListener` refuses a `natsReplicas` above 5, and
  `envoy-infra render webhook-listener-env` a `--nats-replicas` above 5, since the listener
  refuses more at load.

## [21.1.2] - 2026-10-29

### Fixed

- A Mermaid diagram in a Dispatch document no longer restyles the rest of the page, and no reader's
  browser fetches Google Fonts or any address in the diagram's markup for it; the document draws it
  with the same stricter sanitizer, in its own shadow root and off the page's main thread, as
  comments do, so a large diagram no longer freezes the tab while it draws or on each edit. A
  document diagram that takes longer than 5 seconds to draw shows `Unable to render Mermaid
  diagram: it did not finish within 5 seconds` above its source, and an edited diagram redraws once
  typing pauses.
- Reopening an issue closes its document rooms on the server process that reopened it, as closing it does, so a page whose socket was admitted read-only while the issue was closed reconnects and is admitted writable, instead of staying read-only until the person reloads.
- A closed issue's document no longer says "Reload to edit.", which a reload cannot make true; the dashboard shows that line only for a read-only admission that came while the issue was open, while the issue is open and the document's socket is connected.

## [21.1.1] - 2026-10-29

### Fixed

- A bare web address or email in a Dispatch document written through the API (`dispatch doc-edit`,
  an uploaded version) is now a link wherever a comment and the browser editor link it: after
  punctuation (`x;https://…`, `"a@b.test"`), a letter outside ASCII, an escape or a character
  reference, a URL after a digit too, or at a line or table cell opening with punctuation, an email
  there linking to its `mailto:` URL. The server reads an address with both of the editor's readers,
  the one that reads the markdown as written and the one that reads the decoded text it leaves, so
  where the two end an address differently (`x;https://x.test/a_` leaves the `_` out,
  `see http\://x.test/a_` keeps it) each ends it as in the editor. The stored markdown no longer
  carries a backslash before such a URL's colon (`http\://`): the server writes one, or one before
  an `@` or after a `www`, only where plain text would otherwise come back as a link the editor does
  not show, such as `http://localhost`. Plain-text addresses already stored read back as links on
  the next read, as the editor already shows them. A link's destination now has its escapes and
  character references decoded, as the editor reads it (a stored `[a](x\_y)` links to `x_y`, and
  `[a](x&amp;y)` to `x&y`), and a destination holding `>`, a control character or a final
  backslash is written so it reads back. A text `<` that the markdown written after it would close
  into an autolink, such as `&lt;javascript:void(a@b.cc)>`, is written escaped, so no write turns
  it into one link. A text `<` that the markdown written after it would close into inline HTML
  (`Set \<input value=me@x.cc> in the form.`) is now stored with `\<`, so the write reads back and
  no html node replaces the text. An email address in a document stored before emails linked to
  their `mailto:` URL keeps its old link target until the document is written again.

## [21.1.0] - 2026-10-29

### Changed

- `envoy-dispatch census` exempts from its 1 GiB table limit a migration whose every statement is a `create function` (or procedure) in `plpgsql` or `sql`, or a `create trigger`, as Postgres's own parser (libpg_query, through `github.com/wasilibs/go-pgquery`) reads them: each does its work in the catalog and reads no row of the table. No migration is exempt while an enabled event trigger runs on those commands. The report still prints the table's size beside the limit and why the migration is exempt, and the migration's lock holders still refuse it. Every refusal a lock holder causes now carries that holder's diagnosis, read when the census refused: its state, transaction start, `backend_xid`, wait event and query (literals, numbers and comments replaced) from `pg_stat_activity`, and its locks from `pg_locks`.
- Dispatch's migration runner tries a migration again when its lock wait outlasts its five-second `lock_timeout`: it rolls the migration's transaction back, holds nothing for two seconds so the reads and writes queued behind it run, and tries again, up to three attempts (`pgmigrate.RetryLockTimeouts`), logging each timed-out attempt at WARN. A migration that times out on every attempt fails the boot, as one timing out once did before. The lock-timeout error now carries a diagnosis of each session the migration was queued behind, as the census's refusals do. The secrets broker's runner, which applies every migration in one transaction, does not retry.

### Fixed

- `envoy-dispatch census` no longer refuses a young lock holder of its own role as one whose transaction age Postgres hides, naming `pg_read_all_stats` as the fix. Postgres serves a transaction's first read of `pg_stat_activity` until the transaction ends, and the census reads every pending migration in one transaction, so a later migration's holders were joined to activity read at an earlier one: a session idle then showed no transaction start. The census now clears that copy before each read of lock holders and long transactions, measures ages with `clock_timestamp()`, and names `pg_read_all_stats` only for a holder that Postgres hides from the census's role: a session of another role, or an autovacuum worker.

## [21.0.0] - 2026-10-29

### Added

- Every Dispatch `POST` and `PATCH` route takes an optional `Idempotency-Key` request header (INFRA-258). A repeat with the same key and request replays the stored status, headers and body with `Idempotency-Replayed: ?1`; a different request is `422 KEY_REUSED`; and a still-running request is `409 KEY_IN_FLIGHT` with `Retry-After: 1`. A keyed handler finishes detached from its client, is bounded at ten minutes, renews its 60-second lease every 20 seconds, and is counted in shutdown's background wait. A write that committed but lost its answer is `409 KEY_COMMITTED`, so it is never run again. Keys belong to the caller's login and are kept for 24 hours in migration `0137_request_keys`; `POST /api/v1/writes/_test/slow` drives a keyed handler that is slow to finish.

  A write the dashboard pressed runs only as the person who pressed it: every write carrying `Dispatch-Login` is `409 LOGIN_CHANGED` before anything runs when its caller is another person or no person.

  A delivery a process left half-done is finished without anyone asking again. The maintenance pass sends lapsed keyed-send and delivery attempts once under their durable recovery points; migration `0138_pending_delivery_indexes` adds the partial indexes its pending-attempt read uses.
- Dispatch now keeps a browser tab's delivery, event-feed, and agent-view subscriptions on one `GET /ws/live` socket (INFRA-258). Delivery views retain their snapshots, deltas, freshness, and resume semantics; an `events` view sends `events_start` then event frames; and an `agent` view sends responder, replay, and frame events. The server rotates each socket at a random age from 30 to 60 minutes with `4001 max age`; a page reconnects and resumes without a gap. Shutdown and `POST /api/v1/live/_test/disconnect` close with `1012`; loss of identity closes with `1008`; a full hub queue closes with `1013`; oversized and unencodable frames close with `1009` and `1011`; and a disappeared peer is the ordinary `1006` connection loss. `POST /api/v1/live/_test/rotate` exercises the age rotation. The routes this socket replaces are gone.

  `GET /auth/whoami` answers `event_head`, the newest committed event id as of the sign-in check (INFRA-292). A page's first events subscription on `/ws/live` resumes from it, so a change committed between that answer and the socket opening reaches the page instead of being lost until something else refreshed it.

### Changed

- **Breaking:** Dispatch's live routes before `GET /ws/live` are gone (INFRA-258): `GET /api/v1/events`, `GET /api/v1/agents/{session_id}/stream`, `GET /ws/delivery`, and their disconnect test hooks answer `404`. A tab opened before the deploy shows `Live updates unavailable` until it reloads.

  **Breaking:** the body field `idempotency_key` is gone from `POST /api/v1/broadcasts`; the `Idempotency-Key` header names the send instead. A body that still carries the field is `400 INVALID_JSON`.

  **Deploy with no earlier server running:** an earlier server ignores `Idempotency-Key`, so while both versions serve one database a write whose answer was lost can run once on each when its retry reaches the other version. At one replica the Deployment's `Recreate` stops the old pod first; scale a deployment above one replica to zero before this release starts. Releases after it roll again. A rollback to an earlier release brings the hazard back: open tabs, and a `dispatch` client that saw keys kept in the last five minutes, retry a lost write against the earlier server, which runs it again. Fix forward; if you must roll back, stop this release first, knowing a write in flight then may land twice.
- The load test's dashboard share now opens `GET /ws/live` and subscribes to the event feed as a dashboard tab does, in place of the removed `GET /api/v1/events` stream: `run-step.sh --live <n>` replaces `--sse <n>` (same default), and `summary.json` carries `live` (`live_frames`, `live_order_violations`, `live_frame_ms_from_created_at`, `live_frame_ms_from_write_response`) and `live_bytes` in place of `sse` and `sse_bytes`.

### Fixed

- An open document no longer closes its connection and syncs again every 33 seconds while nobody types. Dispatch's document socket (`/ws/doc`) now sends each editor a protocol keep-alive every 15 seconds, an awareness update naming no client, so the dashboard provider and a proxy with an idle timeout above 15 seconds keep a quiet document socket open.
- An issue's `updated_at` never moves backwards: a `PATCH`, claim, or release now keeps the later of the stored value and its statement's clock time, so a dashboard cache does not drop a write that waited behind another issue update. Dispatch also marshals every JSON timestamp in UTC, whatever zone its process runs in.

## [20.1.0] - 2026-10-29

### Added

- Dispatch events say what their write moved, read in the write's own transaction:
  `issue.created`, `issue.updated` and `issue.closed` carry `changed`, the fields whose value moved
  (`closed_at` among them on a close or a reopen), and `ancestors`, the issue's parent chain;
  `comment.created`, `comment.answered`, an issue message's `message.created` and
  `message.answered`, `ask.opened`, `ask.answered`, `ask.resolved`, and an issue event that moved
  the status carry `thread_logins`, whose Replies count the write can move; `user_state.updated`
  carries `changed`, which of `pinned`, `last_read_seq` and `dismissed` moved. Each is absent from
  an event written before this release. A snooze or an un-snooze of an Inbox row
  (`PUT`/`DELETE /api/v1/me/asks/{id}/snooze`), which appended no event, now appends the writer's
  `user_state.updated` when it moves the row, so the writer's other tabs and devices refetch their
  Inbox; its payload is exactly `{login, changed: ["snoozed"]}`, since every signed-in feed
  replays it, so it names no ask, no issue state and nobody else, and wakes no one.
  An events subscription's `events_start` on `GET /ws/live` says `replay_complete: true` when its
  resume is at or below the log's head, which it replays in full (`cmd/dispatch/README.md`,
  "Routes"). `references_changed` names a cited block of a document as
  `{kind: "block", id, ref, artifact_id}` when a write adds or removes a citation of it, and when a
  version of its document changes or deletes it; a document version also names the target of each
  mention it kept in a block whose text it changed. `references_changed` is capped at 100 targets
  of each kind (issue, ask, artifact, block) on its own, where 20 targets of every kind together
  were: past the cap of a kind the event names none of that kind and lists it in the new
  `references_changed_truncated_kinds`, still names the targets of the other kinds, and still sets
  `references_changed_truncated: true` for a reader that predates the kinds.
- Dispatch answers what a rendered `dispatch://` reference shows without the full read:
  `GET /api/v1/issues/{key}/summary` is the issue's `{key, project, title, status, documents}`,
  each document `{id, slug, name, kind, latest_version, primary}` in the order the issue read
  lists them (`primary` marks the spec, which a reference to a block of the spec names), and
  `GET /api/v1/projects/{key}/artifacts/{slug}/summary` a project document's
  `{id, project, slug, name, kind, latest_version}`, resolved as the project document read
  resolves its slug. Neither reads a version body, an approval, a child or an open ask; both are
  open to every signed-in caller, as the full reads are.
- `POST /v1/interests/subscribe` stores the `plugin` and `plugin_version` a registration sends
  (the npm package the session registers through and that plugin's build, trimmed of surrounding
  space) on the session entry, and `GET /v1/sessions` and `GET /v1/registry/<id>` return both. A
  session registered by a plugin that sends neither shows neither key. The `listener subscribe`
  log line names both.
- `packages/envoy/loadtest/` reproduces the load of the 2026-10-26 Dispatch outage on one 2-core machine, so a fix of the commit-order lock or the shared pool is measured before and after on the same load. `run-step.sh <name> --events --sessions --requests` sets up a local stack built from the checkout (Postgres with pgvector from the `pgserver` wheel, a 1 ms latency proxy, `nats-server` with JetStream, the listener and Dispatch, no Docker), seeds it once with 1,000 issues and 8,000 comments, resets it to that seed before every step, and runs sessions on agent and issue topics, five kinds of write at production's mix and GET requests at a route mix read from a file. Each step writes `summary.json`, Dispatch's CPU and heap profiles and a `TABLES.md` with delivery latency from the write's arrival and from its commit, write latency by kind, process CPU and memory, the commit-order lock's holds and waiters, every ungranted Postgres lock with its waiter's and first blocker's statements, the dashboard streams' frame latency and order, transaction ids per second, and latency by route. `run-series.sh` runs the six steps from 0.25x to 10x on one build into one `TABLES.md`. Every endpoint comes from the stack's own `stack.json`, and a step checks that NATS, Dispatch and the listener hold the stack's name and random tokens before its first write, so it refuses the pod's live listener and production; a run aimed at another stack with `-allow-foreign` must name every endpoint and token. Each run marks its writes with its own random number, so it counts only its own commits. One run at a time holds a `LOADTEST_ROOT`: `run-step.sh`, `run-series.sh`, and `stack.sh setup`, `start`, `seed`, `reset` and `stop` on a root another run holds are refused, naming the holder, and a series' running step keeps the root if the series process dies. A step in which Dispatch or another stack process exits fails, naming the process, and its row carries a mark; a read of the run's database that fails shows `-` with a mark, never 0, and a step whose commit rows were not read fails; `meta.json` records how busy the box was just before the step. `packages/envoy/loadtest/README.md` says how to run it, what every column means and how the stack differs from production.
- `GET /api/v1/dashboard/builds` says which dashboard builds Dispatch serves, to any
  authenticated caller: `{serving, live}`, `serving` the entry chunk (`/assets/index-*.js`) of the
  build the answering process serves (`""` when it serves none) and `live` the distinct entry
  chunks of the builds every live server process serves, sorted. The dashboard's update notice
  reads it (Changed, below).
- `DISPATCH_DEV_SIGNIN_SHARED_KEY`, only beside `DISPATCH_DEV_SIGNIN=1` (boot refuses it
  otherwise), gives every dev-sign-in process set to one value one session-cookie key, so a person
  signed in on one of them is signed in on the others. The key is HMAC-SHA256 of the value under
  the fixed label `dispatch dev sign-in`, never the value itself, so a production
  `DISPATCH_SIGNING_KEY` placed there signs no cookie a production server accepts. Unset, each
  dev-sign-in process still generates a key of its own. The Dispatch browser suite's two-process
  mode relies on it: `DISPATCH_E2E_PROCESSES=2` (`bun run e2e:multiprocess` in
  `packages/dispatch`) runs two servers on one database, serving two builds of the dashboard,
  behind a round-robin proxy, and runs the `*.multiprocess.e2e.ts` specs in Chromium; CI runs it
  as the `dispatch-browser-multiprocess` job.

### Changed

- Dispatch's document settlement lists a document's comment and ask marks without computing the
  position of every node, which took a quarter to two thirds of a render on a large spec, and reads
  the marks its comments and asks record through the `comments_anchor_artifact` and
  `asks_anchor_artifact` indexes instead of scanning both tables; the marks it lists, the versions
  it writes and every API response are unchanged.
- Dispatch's `references_changed` also names a cited document or file, as
  `{kind: "artifact", id, ref}` (its uuid and the `dispatch://` address its `Referenced by` panel
  is opened on), so that panel follows a citation made elsewhere. It also names a target whose
  edge the write kept but whose excerpt it moved, which the target's `Referenced by` panel shows:
  an edit of an ask's question or a comment's body that changes the head of its text names every
  target the text still cites, a document version names each target whose stored excerpt it
  changed, and an issue retitle names the issue's parent and the issues it is blocked by. An issue
  write that adds or removes a `blocked_by` entry names each blocker it added or removed, and an
  `issue.created` every blocker it holds, so the blocker's open page keeps its `Referenced by`
  count current.
- A Dispatch tab fetches again only what an event can change. The dashboard's rules
  (`packages/dispatch/web/src/api/event-refresh.ts`) read the events' new fields, so a title change
  on an issue no page shows sends no read of it, a message on another issue sends no Inbox or
  pinned-list read, and a reply in a thread the viewer never wrote in sends no Replies read; an
  event from an older server, without the fields, refreshes what any value could have moved. Every
  refresh goes through one scheduler (`api/query-refresh.ts`): events are batched for 100 ms, and
  each read is fetched at most once a second, never twice at once and never by cancelling the
  request in flight, so a first load's response is kept; an inactive read is only marked stale.
  A test of a read's data waits, while a fetch of it is out, for the response that lands, and a
  read left while it owes a refresh stays stale once that fetch lands, so it fetches when it
  mounts again. An ancestor's detail refreshes on any write of a descendant, whose activity its
  Children tab shows; the Replies page on a retitle of an issue it lists; the Architecture tab on
  any write of an issue its tree lists; a document's `Referenced by` panel when the event's
  `references_changed` names the document, and when an ask or comment that document owns or
  holds the anchor of is written, re-anchored or edited; and the whole Inbox on the viewer's own
  snooze in another tab. An event whose `references_changed_truncated_kinds` names the kinds it
  cut refreshes every read of those kinds alone (every issue detail and issue panel, every ask
  read, ask list, ask panel and the Inbox, every document panel, every block read), beside the
  targets of the other kinds it still names; only a truncated event without the kinds refreshes
  every count and panel. A read the server refuses with a 503 carrying `Retry-After` is retried
  after that many seconds, at most 30, plus a random share of them again, so tabs refused together
  do not all come back at once and a proxy's day-long `Retry-After` still retries within a
  minute; any other failure is still retried after 500 ms.
  The app's query client (`api/query-client.ts`) gives every mutation's invalidation the same
  never-cancel rule. A resume of the event feed whose replay Dispatch calls complete
  (`replay_complete`) refreshes nothing beyond what its events change, where every reconnect, a
  tab regaining focus among them, refetched every read. A `dispatch://` reference reads the new
  summary routes in place of the full issue, and an inline link reads no document text. A
  reference's summary or block read is fetched again on a window focus only after it failed, so a
  503 past its retries recovers on the open page.
- A comment's margin-record write (a new anchored comment, a reply, a resolve, a reopen, an edit,
  and an accept's or a reject's projection) no longer reads and renders the document a second time
  or walks every anchor mark to weigh it, and the room's update observer no longer renders the
  publish of a margin write forked from that same room. Responses and stored state are unchanged.
- A cold read of a Dispatch document now encodes the token representation of its tree once, into
  one buffer held for that call alone, and takes the document's token and every block's token from
  that one encoding, where it encoded each block again apart (a nested block once per enclosing
  block) and then the whole tree once more. It lists the blocks inside each table without
  measuring every node's position, and reads a mark's name from its Y.Text key without a regular
  expression. Tokens, block lists, markdown and responses are byte for byte unchanged, errors
  included.
- A write to a large Dispatch document costs less CPU and allocates less. Every walk of a
  document's tree (each anchored comment's refresh on a write, each find, each edit) now counts a
  text's UTF-16 length without copying it and measures each subtree once, where it copied every
  text twice and measured each node again for each of its ancestors; an insertion measures the
  children only of the nodes whose range holds its position. On a replay of a production write
  load (3 runs each side) the server used a median 236 CPU-seconds where it used 368 (−36%) and
  allocated 48 GB where it allocated 89 GB (−45%). On a 427 KB generated spec one walk takes 94%
  less time and allocates 94% fewer bytes (`BenchmarkPositionedWalk`), and its time no longer grows
  with the document's depth. Positions, sizes, quotes, tokens, rendered markdown and API responses
  are unchanged.
- A Markdown document now nests at most 32 typed blocks inside one another, whatever other blocks
  stand between them, within its 100 blocks. A document write refuses a typed block opened inside
  32 typed blocks, `400 INVALID_MARKDOWN` with `line N opens a typed block inside 32 typed blocks;
  a document nests at most 32 typed blocks`, and a write whose own markdown nests within the bound
  but lands deep enough that the result would nest past it is refused with `the result nests N
  typed blocks inside one another` (an accepted suggestion's `INVALID_OP` on `replace_with`). 32 is
  where the browser's parser stops opening `:::` blocks, so the server stores no document the
  browser would read differently.
- A write to a Dispatch document with many open anchored comments and asks no longer costs a walk
  of the whole document per anchor. Every edit, upload, version and new anchored comment refreshes
  the open anchors from one walk (`pmdoc.FindMarks`) with the same quotes and rows as before. The
  refresh now lists its rows in a stable order, the open asks and then the unresolved comments,
  each by creation time and id, so one refresh announces its `*.anchor_refreshed` events in the
  same order every time; before, the listing had no order. An upload re-anchors every row with
  one quote finder and one mark writer, and a miss it drops no longer ranks the nearest blocks. A
  new anchored comment or ask reads its anchor off the tree its write already read, and
  `MarkRange` and `Unmark` read the document's text ranges in one pass. On a 150 KB document a
  doc-edit, an anchored comment and an upload cost the same at 80 open anchors as at 10.
- A write to a Dispatch document no longer renders the whole document before the write when that rendering is already known: the first operation of a write to a resident room takes the room replica's rendering when nothing but the write's own updates reached the replica since its fork was read, and a later operation of the same transaction takes the previous operation's. The room's update observer no longer renders the document again after a published write's update: it takes the write's own rendering on the same condition, and it classifies an update the room already held when it loaded (a write published into a room that its publish loaded) as unchanged. Every rendering, change record and ask-block record is the same as before. On a replay of a production write load (GOMAXPROCS = 2) the server used 31.5 s less CPU in an 8-minute step, 427.0 s against 458.6 s (6.9%, about 0.065 cores).
- Every Dispatch server process now stores its dashboard build's assets in Postgres at boot,
  before it listens (migration `0136_dashboard_assets`: `dashboard_builds`, `dashboard_assets` and
  `dashboard_build_assets`), and answers a file under `/assets/` that its own image lacks from
  there, through a cache of at most 64 MiB its requests share, with one database read per file
  however many requests ask for it at once. While a rolling update or a rollback runs two builds
  behind one load balancer, a page of either build is served its chunks by whichever process
  answers, so a tab no longer gets a failed chunk or a blank page from the process that did not
  build it. A restart on a build already stored writes no bytes, and each boot then prunes every
  build no live server process serves that is not among the 20 started most recently. A publish
  that fails, a build holding a file over 8 MiB among them, refuses the boot, so a rolling update
  keeps the processes it would have replaced; `bun run check:asset-sizes` in `packages/dispatch`,
  run in CI after the build, refuses such a build first. A release before `0136` neither reads
  nor writes the new tables, so it serves beside this one unchanged, its pages' chunks from its
  own image alone as before, and rolling back past `0136` is an image rollback.
- **Breaking:** `DISPATCH_ASSET_STORE_BUCKET` and its S3 reader of earlier builds' assets are
  removed: a Dispatch that still sets the variable refuses to start, naming it and saying that
  every server process now keeps its build's dashboard assets in Dispatch's database. Unset it
  before deploying this release.
- A dashboard tab now recovers from a failed script, module preload or stylesheet of its build,
  the main stylesheet included, by reloading while fewer than 3 of its recovery reloads fall in
  the last 5 minutes, where it reloaded once per tab: a tab open across many deployments recovers
  from each, and a page whose assets fail on every load still stops after three reloads. The
  inline script in `index.html` and the lazy-chunk recovery keep the reloads' times on one
  sessionStorage key, `dispatch.chunk-reloads`, and share the budget; the once-only
  `dispatch.reloaded-for-chunk` key is neither read nor written.
- The dashboard's "A new version is available" notice now asks `GET /api/v1/dashboard/builds`
  once, on focus at most once a minute, where it compared `/healthz` and a fresh `/`, and offers
  itself only when `live` names exactly one build and it is not the tab's own: while a rolling
  update or a rollback still runs two builds, no tab is told to reload. A `404` from a process
  older than the route, a `401`, a `5xx` or a network failure offers nothing.
- After a dropped connection, the dashboard's first reconnect of the workspace event stream, an
  agent's live stream, the Delivery and Pipeline socket and a document's socket waits a random
  time under 2 s, so the tabs one restart or roll dropped reconnect, and refresh their reads,
  spread over two seconds rather than in one burst. Later attempts back off exponentially with
  equal jitter, half of `min(1 s × 2^n, 30 s)` plus a random share of the other half, where the
  streams waited that whole delay and the Delivery socket ±25 % of it. Dispatch's restart close
  (1012) no longer reopens the Delivery socket at once but waits that first spread, and a
  document's socket that had opened reconnects after the spread too; its first connect is still
  immediate, and `online` or a tab turning visible still reconnects the event stream at once.

### Fixed

- A person's reply in a Dispatch comment thread now reaches the agents in that thread (#2366). A
  reply stores its thread root as `reply_to`, so the outbox sent it to the root's author and the
  direct parent's author, which were both the person when a person started the thread, and the agent
  that answered in the thread never heard the person's next reply. The outbox now sends a person's
  reply, and its edit, to every session that wrote a comment in the thread and to every session that
  wrote the document the thread is on. It reads these sessions for a whole batch in one query
  (`outbox/recipients.go`). On an issue with no route and no session claim (unclaimed, or a person
  holds the claim), a person's comment, and its edit, now reach every session that wrote the issue's
  primary document (its creator and every version's authors), in an ask's thread too, so a session
  that wrote the spec without a claim still hears comments on the issue. The outbox reads these
  sessions for a whole batch in one query too. The claimant's copy is the claimant route of #2225,
  which these copies join: a session reached more than one way gets one copy. A thread session or a
  spec session the comment's mentions resolved to (its `mentions`, as the claimant and follower
  routes read them) hears it once, from the mention, once the mention was delivered; one whose
  mention failed still gets the outbox's copy. A resolve or a reopen reaches none of the thread,
  document or spec sessions this change adds; it reaches the route, the claimant and the thread
  root's author as before, so the reopen that a reply makes in the same transaction tells no new
  session twice. When a batch's recipient query fails, the comment's owner topic, route and claimant
  are still sent, and the event is retried for the rest, or released when the process is stopping.
  A `comment.created` payload no longer carries `suppressed_authors`: the session routes read the
  comment's `mentions`, and nothing read the field.
- A leased periodic loop (webhook redelivery, suggestion outcomes, document compaction, reference excerpts, the block index, edit notices, the issue-ask repair, the architecture import and the delivery reconcile) counts each tick in the period it was due in, so a lone process no longer loses a period when a tick's lease or its period query is late.
- The live event stream reads the event log's cursor check in one statement, so a reset of the log between two reads is no longer taken for a second reset that closes the next subscription.
- A Dispatch body that nests `:::` blocks deeply no longer freezes the reader's browser tab. The
  browser's parser, micromark-extension-directive's container construct, read a block's content
  once for each block around it: a comment of 2,000 nested unclosed `:::a` lines and one long line
  held the page for about a minute, and one of 1,000 nested blocks, closed again, about 15 s. A
  construct of `@legion/proof-editor`'s own reads that content once, and an opener inside 32 open
  blocks reads as the text it is, so each of those bodies now draws in well under a second, in
  every surface that shows one and in the document editor. A body nesting 32 blocks or fewer reads
  as before, with three exceptions, since the browser now opens a `:::` block as it opens a quote
  or a list: after a leading `---` that no later line closes, a `:::name{...}` line opens no typed
  block at the document's level and reads as text, as a list, a quote or a footnote definition
  there already did, and a document write reads it so too; a footnote definition inside a `:::`
  block also defines its footnote for a call in the text before the block, so a document write no
  longer refuses a footnote definition inside a typed block and reads it where it is written, as
  it reads one inside a quote, when it holds paragraphs alone and no list item stands around it;
  one with a list item around it, or holding any other block, whose spacing the browser reads
  otherwise, is still refused; and a link reference definition inside a `:::` block also defines
  its reference for the text before the block, so `[r]` before a block holding
  `[r]: https://u.example` now shows as a link where it showed as the text `[r]`. A document write
  refuses a link reference definition, so no stored document reads differently, only a body that
  is displayed. A rule opening a document that holds a typed block at its level is now written
  `***`, so it no longer reads back as an unclosed front-matter opener that turns the typed block
  into text.
- A document write now reads an unclosed code fence inside a typed block in a list item, a quote
  or a footnote definition as the browser does, without the trailing newline it kept from the
  blank line its container ends at; one in a quote that a last `>` line with no line feed ends
  keeps that blank line, as the browser keeps it.
- A document write now reads the spacing of a list and its items around an unclosed code fence
  inside a typed block in a list item, a quote or a footnote definition as the browser does.

## [20.0.0] - 2026-10-28

### Added

- `envoy-infra:gcp:Dispatch` takes `appEphemeralStorage` and `proxyEphemeralStorage`, the
  Kubernetes quantities that set the `ephemeral-storage` request and limit of the app Deployment's
  `envoy-dispatch` container and of its `cloud-sql-proxy` sidecar: the disk a container's writable
  layer, temporary files and logs may use before the kubelet evicts the pod. Each unset renders
  nothing and Autopilot gives that container its default `1Gi`, so a deployment that leaves both
  unset renders the pod it always had. The front door and the listener are unchanged. The
  component refuses, naming the inputs, a value that is not a quantity, is empty or is not above
  0, and a pair whose sum, an unset container counted at Autopilot's `1Gi`, is below Autopilot's
  `10Mi` pod floor or above the `10Gi` ceiling it rejects (so `appEphemeralStorage` at most
  `9216Mi` beside the proxy's default).
- A pull request's `synchronize` payload carries `before`, the head GitHub's webhook says the
  branch moved from, beside `head_sha`, and every pull request and push payload carries `sender`,
  the login of the account GitHub says triggered the event. The Legion daemon reads the move a
  head arrives by from `before`, and which push made it from `sender`; a listener without them
  publishes none, and the daemon then judges that arrival as it did before.
- The ask reads name who besides the followers a person's answer and replies reach:
  `GET /api/v1/asks/{id}` and every ask thread (the Inbox rows and an issue's
  `GET /api/v1/issues/{key}/asks?thread=1`) carry `claimant`, the session holding the claim on the
  ask's issue, `route`, the issue's route (`session:<id>` or `role:<name>`), and that route's
  reach as the issue reads carry it: `route_status` (`live`, `no_holder`, or `unknown` when the
  listener is unconfigured or did not answer) and `route_holder`, the live session the route
  reaches (the routed session while the listener lists it, or the role's holder), resolved per
  read; each is null when absent and on a project document's ask. The Inbox ask card's `Reaches N`
  panel lists the claimant (`via claim`) and the route (`via route`) beside the followers and
  subscribers, and counts each session once: a session that is the claimant, the route's holder, a
  follower or a subscriber at once is one row and one in `N`. While the route's reach is unknown
  its row carries the issue page's `Route reach unknown` marker: a `session:` route's row shows its
  session's running state, from the agent registry, beside the marker and still counts the
  session, and a role route reads `Reaches N + route (unknown)`. A role route nobody holds shows
  the issue's `Nobody holds it right now` marker, `UnreachableRouteMarker`.
- `DISPATCH_SIGNING_KEY` takes one key or several separated by whitespace, parsed as
  `DISPATCH_AGENT_TOKEN` is (startup refuses an empty or repeated entry, naming its position and
  never its value; a value of nothing but whitespace counts as unset). The first key signs every
  new session cookie and the sign-in's state cookie and seals every refresh token Dispatch stores;
  every key verifies a cookie and opens a seal. The seal's `v1:` format is unchanged byte for byte.
  So the key rotates without signing anyone out, in three steps each rolled to every server
  process: add the new key second, move it first, then remove the old key no sooner than 30 days
  later, the cookie's lifetime; a refresh token sealed under the old key is sealed again under the
  new one at its person's next hourly confirmation.
- A registry of live Dispatch server processes (migration `0135`, tables `server_processes` and
  `server_build_announcements`, both new). A process registers once its port is bound and before
  it serves, stamps its row every 5 s on a one-connection pool of its own, counts as live while
  that stamp is within 15 s of the database's `now()`, and deletes its row once its HTTP drain has
  ended; rows a process left behind are pruned 10 minutes after their last stamp, under the lease
  `server-processes`. Operator subcommands never register. A process whose signing key set a live
  process does not share both ways (its current key among the other's keys, and the other's
  current key among its own) logs `dispatch: register this server process` at ERROR, naming the
  other process and the rotation steps, and exits 1 before it serves: a process that minted a key
  in its data dir refuses beside any live process, and so does a pod rolled with a changed secret
  beside pods that hold only the old key, which stalls the roll while the old pods serve. Before
  it exits, a refused process logs at WARN which live process refuses it and waits up to one live
  window (15 s) and a heartbeat for that process to stop heartbeating, trying again every
  heartbeat, so a restart after a kill (a dev sign-in server, whose key is minted per process, or
  a pod replaced with a changed secret) starts once the dead process's row lapses. A SIGTERM or
  Ctrl-C during that wait logs `dispatch: stopped while waiting for admission to the registry of
  live server processes` at INFO and exits 0, since the process served nothing. A
  process whose heartbeat lapsed (frozen, cut from the database) or whose row was pruned checks
  its keys again before it counts as live once more, and beside a live process whose keys it does
  not share logs `dispatch: this server process no longer shares its signing keys with a live
  server process` at ERROR, stops serving after its HTTP drain, and exits 1. The registry's pool
  is one more Postgres connection per process, and its prune one more lease connection, twelve.
- Dispatch's `/healthz` reports `fleet_commit`, the commit of the latest announcement that every
  live server process serves one build (`null` when there is none, or before this process's
  announcer has first read it; up to one five-second pass behind), and `signin` and
  `service_tokens`, whether the sign-in issuer's and the service-account token issuer's discovery
  documents have been read (`null` where that issuer is not configured). None of the three
  decides the status.
- `dev-oidc-issuer --code-flow <client id>=<client secret file>` serves the authorization code flow
  for that one client, so a browser signs in to a local Dispatch with no Google: each
  authorization signs in the next person queued at the loopback-only `POST <issuer
  URL>/_dev/sign-in-as` (a JSON object of ID token claims), or `--sign-in-as <email>`'s person when
  none is queued, a member of `--sign-in-group <group>`'s group when that is given. Dispatch takes
  `DISPATCH_SIGNIN_DOMAIN` only under Google's issuer, so a local Dispatch signs in through this
  issuer with `DISPATCH_SIGNIN_GROUP`.

### Changed

- A person's answers, comments and messages on a claimed issue reach the session holding its
  claim, on its own `notifications.agent.<session_id>` topic, where before they reached it only
  when it had subscribed to the issue, was its route, followed the ask or wrote the commented
  document. A person's `ask.answered`, `ask.handed_back`, `ask.resolved`, `comment.created`,
  `comment.edited`, `comment.resolved`, `comment.reopened`, `message.created` and
  `message.answered` take this claimant route; a message a person sends to one session still
  reaches only that session. The claim is read when the event is published, so a retry after the
  claim changed hands reaches the new holder, and a claimant that is also the route, a follower or
  an author gets one copy. A claim or a `session:` route brings a person's answers, comments and
  messages, and a route a person's other changes on the issue too; a subscription to the issue
  brings other agents' activity, beyond a reply in an ask the session follows.
- A new comment (`comment.created`) is left to its mention only once the mention delivered it: the
  route, claimant, author and ask follower copies skip a session the comment's mentions resolved
  to only when `comment_deliveries` records the mention `sent` to that session, so such a comment
  arrives once, as the mention. While the mention's send is still pending (under a minute old),
  the outbox publishes every other destination, holds that session's copy back and tries the
  event again a few seconds later without counting an attempt; when the send failed, was
  abandoned or was never recorded, the session gets its own-topic copy as if it had not been
  mentioned. This changes the route and author copies too: before, a mention resolved to the
  route's session or a document's author suppressed that copy even when the mention's send failed,
  so the comment reached that session directly by no path. The ask follower route, which before
  reached every follower, now skips a follower the mention delivered the comment to, by the same
  rule. A comment's edit, resolution and reopening are never left to a mention and reach every one
  of these sessions, and are not held back with a deferred `comment.created`, so a session can
  receive a comment's edit before the comment while its mention's send is undecided. One copy is
  not guaranteed: a mention whose receipt timed out is recorded `failed` though the listener may
  have delivered it, and a first attempt stranded past the minute is abandoned though it may have
  landed, so the session also gets its own-topic copy, under a different dedupe key; a host treats
  the second copy as the same comment. The pending window is `model.DeliveryClaimLapse`, the one
  lease the API's delivery claims and the outbox's mention deferral both read.
- **Breaking (a hand-written signing key holding whitespace):** a `DISPATCH_SIGNING_KEY` with a
  space, tab or line break inside it is now several keys, the first of which signs, so the first
  start of this release signs everyone out once and leaves the refresh tokens sealed under the old
  value unopenable, where the release before read it as one key. A generated key (the
  `envoy-infra:gcp:Dispatch` component's 64-character alphanumeric value, or the data dir's
  base64url file, which is read whole) is unchanged. This release is never the release of a first
  rotation: an earlier build reads `"K1 K0"` as one key, so no build older than this one is rolled
  back to while the setting holds two values.
- **Breaking (`server.started` consumers):** Dispatch publishes `server.started` once each time the
  commit every live server process serves changes, not once per process start: a rolling update
  announces once its last process of the earlier build has gone, a restart of every process on the
  announced commit announces nothing, and a rollback announces the earlier commit again. Whichever
  process finds the fleet agreeing records the announcement in `server_build_announcements`, under
  an advisory lock, and any process whose bus is connected publishes it, so one whose NATS is down
  leaves it to another. Subject and payload
  are unchanged; `started_at` is the database's time of the announcement, and the event id, source
  event id and dedupe key are `dispatch-server-started-<commit>-<seq>`, `seq` the announcement's
  own, where they ended in the start's Unix time. Builds before this one do not register: the
  first release that carries it announces as soon as its own processes agree, and a process of an
  earlier release started meanwhile still publishes its own start, so that release is deployed with
  no earlier process serving beside it.
- Dispatch's `/healthz` is `200` while the database answers whatever NATS does: `ok` says this
  process can serve requests, and `nats` still reports the connection (`null` when disabled)
  without deciding the status. Every NATS client of the server pings every second and declares
  the connection lost at two pings outstanding, so `nats` reads `false` within two to three
  seconds of a silent cut, where it took minutes. The probe is a readiness probe and never a
  liveness probe: it fails while the database is unreachable, so a liveness probe on it would
  restart every server process during a database failover.
- Dispatch starts and serves while NATS is unreachable or refuses its credential. Each of its NATS
  clients connects in the background and retries forever, a refused credential logged at ERROR at
  most once a minute; only a configuration error (an unusable seed or SOCKS5 setting, a server
  that is not this machine's) still refuses the boot. The stream is reconciled on the first
  connection, the outbox publishes what was written meanwhile, the webhook redelivery sweep opens
  its KV bucket on its first pass with NATS connected, and the agent conversation route answers
  `503 STREAM_UNAVAILABLE` until the relay connects.
- The Dispatch auto-deployer (`deploy/scripts/autodeploy.sh`) takes a new image as healthy only
  once `/healthz` answers `200` with `nats` not `false` (`null`, NATS turned off, passes), within
  the same 90 seconds. Dispatch now serves while its NATS connection fails, so a `200` alone would
  keep an image whose NATS credential is refused; such an image is rolled back, as it was when it
  exited at boot. A NATS outage fails the previous image's check too, so its rollback then reports
  that it did not become healthy.
- Dispatch starts without its sign-in issuer or service-account token issuer. Each discovery
  document is read in the background from boot (retrying after one second, doubling to thirty,
  each attempt bounded at ten seconds and at least five seconds after a failed one, a failure
  logged at ERROR at most once a minute) or by the first request that needs it. Until then
  `/auth/start` and the callback answer `503 SIGNIN_UNAVAILABLE`, a JWT-shaped bearer
  `503 OIDC_ISSUER_UNAVAILABLE` rather than `401`, and agent tokens are served; a plain-text
  refresh token the boot cannot revoke yet stays for the next boot.
- `/auth/start` no longer answers `429` "too many pending sign-in attempts": no process holds a
  sign-in in flight, so there is nothing to cap.
- A settlement of a Dispatch document that wrote nothing into the room and repaired nothing in
  the tree it read now renders that tree once, where it rendered it twice: its version reuses the
  first rendering, block offsets included. Versions, their block index and the reference
  excerpts are what they were.
- An issue's references answer, `GET /api/v1/issues/{key}/references`, now parses each
  document's creator once and each distinct version author list once per request, where it parsed
  both again for every version. The answer is unchanged.
- The listener's CI summary loop no longer copies and sorts the whole CI cache every second. It
  runs a pass only on the one-second tick at which a record changed or a record it waits on falls
  due (its debounce runs out, its claim goes stale or its handover grace ends), so with nothing
  changed it does no work, and a change costs the same at any cache size. Settlements, retries,
  the `envoy_ci_legacy_records_held` gauge and its `checks held back` log line keep their timing.
- `GET /api/v1/agents` no longer asks the Envoy listener on every request: it reads Dispatch's shared copy of the listener's session list, the one issue reads' `route_status` reads, so its list is at most 5 s old and one listener call per 5 s serves both. Its rows, fields and order are unchanged. A listener slower than 2 s now answers `503 ENVOY_UNAVAILABLE` where the client used to wait up to 5 s, and after a failed listener read an agents list answers that `503` for up to 5 s without asking the listener again. The new test-only `DISPATCH_TEST_ROSTER_UNCACHED=1`, under `DISPATCH_TEST_HOOKS=1`, turns the copy off for an end-to-end test process, so every read asks the listener.
- **Breaking:** `envoy-infra:gcp:Dispatch`'s Cloud SQL instance runs PostgreSQL 18 by default:
  `databaseVersion` now defaults to `POSTGRES_18`; envoy-infra v2.9.1 and every release before this
  change defaulted to `POSTGRES_16`. A stack that leaves `databaseVersion` unset is upgraded in place
  from its current major version to PostgreSQL 18 at its first `pulumi up` after its `envoy-infra`
  pin moves to a release that carries this default: the first `envoy-infra-v*` tag cut from a `main`
  commit that contains sjawhar/legion#2364. That is a Cloud SQL major-version upgrade: Cloud SQL
  makes the instance unavailable while it performs the upgrade, so Dispatch is down for that time,
  and the upgrade cannot be reversed in place. Cloud SQL makes a pre-upgrade backup immediately
  before it starts, and the way back to the earlier version is restoring that backup to a new
  instance running it
  ([Upgrade the database major version in-place](https://cloud.google.com/sql/docs/postgres/upgrade-major-db-version-inplace)).
  To keep PostgreSQL 16, set `databaseVersion: POSTGRES_16` before moving the pin.

### Fixed

- An artifact file upload no longer leaves its spooled part on the pod's disk. The multipart
  parser spools a file part past a megabyte to a temporary file; `net/http` removes the files of
  the request it hands a handler, but the upload parses a copy the access log and the transaction
  tracking made with `WithContext`, so the server never saw them, and every such upload left its
  file behind until the pod's `ephemeral-storage` limit was reached and the kubelet evicted the
  pod, taking Dispatch down until a replacement pod was ready. The upload removes the files itself
  when it returns, whether it stored the file or refused the request after parsing it.
- A sign-in started on one Dispatch server process completes on another. The sign-in in flight
  (state, nonce, the page it returns to, its expiry ten minutes on) rides the browser's
  `dispatch_oauth_state` cookie as `s1.<payload>.<signature>`, signed under a key derived from
  the current signing key for that purpose alone and verified under every key, where it was kept
  in the memory of the process that started it, so a callback reaching another process answered
  `400` "invalid or expired state". The processes need one `DISPATCH_SERVER_URL`, since the
  callback URL is built from it. A sign-in started on the release before this one and completed
  on this one fails once, and the person starts again.
- An outage of the sign-in issuer no longer signs people out. Only a refresh the issuer refuses
  (`400` or `401` with the OAuth error `invalid_grant`), or one whose ID token no longer passes
  the sign-in rule, ends a person's sessions; any other failure (no answer, a timeout, a `5xx` or
  `429`, another OAuth error, an answer without an ID token that verifies, an issuer not yet
  discovered) refuses the request `503 SIGNIN_UNAVAILABLE` and keeps the sign-in, so the first
  request after the issuer answers refreshes and is served. Before, any failed refresh ended every
  session of the person as their hour lapsed.
- Dispatch's time-gated writes use the database's clock rather than the serving process's: a
  sign-in's confirmation (`people.confirmed_at`, and the hour it is measured against), a delivery
  pull request's `last_event_at`, `deployed_event_at`, and the cutoff of the delivery changes
  prune, so server processes whose clocks differ agree on them.
- An ask block whose answer selects nothing is now written `selected="[]"` in a version that a
  settlement made from the tree it repaired, as the room holds it, where it was `selected="null"`.
  It happened when a peer wrote the answer into the block before the settlement's own repair
  did, and the next settlement wrote a version again that changed only that attribute.
- Closing an issue withdraws its open asks, and reopening it brings them back. Every
  close through `PATCH /api/v1/issues/{key}` - a person's Close or board drag,
  `dispatch issue-update --status done`, the Legion daemon's close when a pull request merges -
  withdraws every open ask of the issue, whoever's turn it is: decision blocks in its documents,
  plain asks and requests to approve one of its documents. Each is recorded `resolved` with
  resolution kind `withdrawn`, the closer as its actor and the reason
  `Withdrawn by the close of <KEY>.`, and gets one `ask.resolved` before `issue.closed`; its
  block, and each copy in another document of the issue, reads `state="resolved"` in the live
  document, with no version written. Asks answered or resolved before the close are left as they
  were, and a withdrawn approval request writes no review. The close first settles the issue's
  documents that owe a settlement, so a decision block written seconds before the close is
  withdrawn with a record. A reopen opens again exactly the asks the close withdrew, each with one
  `ask.opened` before `issue.updated`, and they return to the Inbox. An ask resolved in the moment
  between the close's settlement and its taking the issue keeps its resolver's record, and the
  close's sweep writes each copy of its block in another document of the issue `resolved`, as the
  copy's own settlement would have, since none runs while the issue is closed. An ask answered in
  that moment keeps its answer, and the sweep writes each such copy `state="answered"` with who
  answered and when but not the answer itself, which the reopen's settlement writes in.
  `withdrawn` is the server's alone: `POST /api/v1/asks/{id}/resolve` still takes only `retracted`
  and `resolved`, and any other kind is `400 INVALID_RESOLUTION` saying so and naming the kind it
  got; an ask on a closed issue is still `409 ISSUE_CLOSED`. A repair pass,
  `api.RepairIssueAsks`, runs at boot (bounded at 30 s) and then every 10 minutes under the lease
  `issue-ask-repair`: it withdraws the asks left open on issues closed before this release, each
  naming its issue's earlier closer, restores the withdrawn asks of an issue a process of the
  earlier release reopened during the roll, and logs `withdrawn`, `restored` and `swept` counts.
  A pass that finds nothing to change writes nothing: it appends no update to any document and
  leaves each document's owed settlement marked when it was, so an interval pass does not sweep
  the closed issues' documents again. An issue whose repair fails no longer holds that bound back:
  each interval pass moves it and carries the issues it did not repair into the next pass, so the
  next pass retries them and sweeps only the other documents marked since. A document of the issue
  whose stored history cannot load is left as it is and logged, so the close, the reopen and the
  repair go on without it rather than answer `409 DOCUMENT_UNLOADABLE` until it is rebuilt.
  The dashboard, which ships in this server, shows a withdrawn decision block on a closed issue's
  current document with the badge `Withdrawn` and `Withdrawn when <closer> closed the issue`, as
  the activity feed's `ask.resolved` line reads; a saved version shows its own attributes, so a
  later withdrawal never appears on it.
- A document write whose transaction changes nothing appends no update, refreshes no anchors and
  leaves the document's owed settlement marked when it was, for every writer. ygo hands
  a document's observers an update for every transaction, one that changes nothing included, and
  every such update used to be appended and to mark the document owing a settlement again.
- A closed issue's document no longer tells a person who typed nothing that "1 edit saved in this
  browser can't be sent: this document is read-only". The editor stamps each heading's
  id from its text as it loads, and the dashboard stored that stamp as an unsent edit even on a
  read-only document. The dashboard now never counts an update that changes only what the editor
  derives (a heading's id, an ordered list item's label and list type), whatever the order of a
  reconnect, a close and a reopen. It holds such an update in memory and stores it only ahead of a
  later edit a person makes in the same client, so that edit's stored row still applies after a
  reload. It drops a stored row an earlier read-only load left that changes nothing; an edit a
  person typed while the document was writable is still kept and named.
- A Mermaid code block in a Dispatch comment, reply, message or ask draws as a diagram, as it
  already did in a document, with its source behind a `Mermaid source` disclosure; a source the
  renderer refuses shows `Unable to render Mermaid diagram: <reason>` above its source. Before,
  every such block showed as its source code. The diagram's styles stay inside it, no reader's
  browser fetches Google Fonts or any address an author writes into the diagram, and a page with
  no document open loads the renderer only once a body holds a diagram. The renderer runs off the
  page's main thread, so the page keeps responding while a diagram draws, and a diagram that takes
  longer than 5 seconds to draw shows `Unable to render Mermaid diagram: it did not finish within
  5 seconds` above its source.
- A document written through the API or the command line now reads `\&amp;` as the text `&amp;`,
  as a comment and the browser editor do, where it showed `&`. The server removed every backslash
  and then decoded HTML's character references by HTML's rules, so it also decoded a reference
  without its `;` (`AT&T &copy 2024` showed `AT&T © 2024`), one with too many digits, a legacy
  name used as a prefix (`&notit;`) and one naming a control character or a noncharacter, which
  the editor shows as written or as U+FFFD; and it read `&#10;` in text as a line break where the
  editor shows a space. Text, link and image titles, image addresses and footnote labels now read
  as the editor reads them, and a link or image title or an image address holding `&amp;` is
  written so that it reads back as `&amp;`. Documents already stored keep the text they hold.

## Before changelog fragments

### Added

- `envoy-dispatch` serves `GET /livez`, answering 204 with no I/O, for liveness and startup probes
  that must not wait on or fail with Postgres, NATS or the file store; `/healthz` is unchanged.
- The listener answers `POST /v1/replay` with the messages `ENVOY_NOTIFICATIONS` stored for a
  session after a cursor per subject, so a session that receives on its own NATS subscriptions
  (pi-envoy, claude-envoy) reads back what it missed while they were down. It serves only the
  session's own `notifications.agent.<session_id>`, exactly, and the topics registered as its
  interests that lie inside a shared stream subject (`notifications.dispatch.>`,
  `notifications.github.>`, `notifications.slack.>`, `notifications.ghostwispr.>`,
  `notifications.whatsapp.>`), whose publishers never carry one session's private payload. Every
  other subject is `refused: "not_followed"` even when the interest store holds it: another
  session's agent subject or a wildcard over them, every exception lane (the session's own
  included), the Legion controller's `notifications.legion.>` notices, and a wildcard wider than
  one shared subject such as `notifications.>`. Every subject of the stream is classified shared
  or private beside the stream's subject list, and a test fails on a new one left unclassified. An
  answered subject the deployed stream does not retain is `refused: "not_retained"`; a subject
  without a cursor gets the stream's last sequence to start one. The answer's `stream` names the
  stream's `id`, the `envoy_stream_id` metadata the listener and Dispatch's server set once (on the
  stream they create, or on a deployed one without it, whose other metadata every start keeps; a
  deployed stream that needs no other change gets the id alone, its replicas and every other
  setting kept as deployed), which a backup and restore, a reconcile and a leader election keep,
  while a restore changes `created`, and after a clustered restore the servers can report
  different `created` times, so a later leader election can change it too. One answer holds at
  most 32 subjects, 500 messages (100 by default), 2 MiB and 5 s of reading, and says `more` for
  a subject it cut short. NATS refusing the read is `503 {"code":"REPLAY_UNAVAILABLE"}` naming
  NATS's error; a listener without the route answers 404, which a session reads as "does not
  replay". A `session_id` that is not one literal NATS token (`.`, `*`, `>`, whitespace or a
  control character) is a 400, so a wildcard cannot make `notifications.agent.>` its own subject
  and read every session's inbox (INFRA-439).
- `envoy-infra`'s `ListenerPermissions`, and through it `HostPermissions` and
  `DispatchPermissions`, grant `$JS.API.STREAM.MSG.GET.ENVOY_NOTIFICATIONS`, the stored-message
  read `/v1/replay` makes; it reads one message at a time and can create, change or delete
  nothing, and `SessionPermissions` does not get it. The rendered users change, so moving a hub
  with a `listener`, a `machineId` user or a `dispatchMachineId` user to this release replaces its
  VM once, with NATS down until the new VM starts; a hub with none of them keeps its VM. A
  listener whose NATS user lacks the read answers `/v1/replay` with `REPLAY_UNAVAILABLE`
  (INFRA-439).
- A person's browser edit of a document reaches the agent sessions that wrote it. Once the
  document has been quiet for 30 seconds after a person's last settled version, Dispatch appends
  one `artifact.human_edited` event: the person who edited last is its actor, and its payload
  names the two versions the quiet window spans, each person who edited, and the unified diff of
  the two versions, cut at its last line end within 4,096 bytes. It goes to the issue's route and
  to each session the document credits, never to a person. The quiet window is a row of the new
  table `document_edit_notices` (migration 0129), so a notice still pending when a process stops
  is sent by the next sweep of either process; the sweep runs every 5 seconds under the
  `edit-notices` lease, and the lease pool grows from nine connections to ten for it. A notice
  on an issue closed inside its quiet window is dropped. The dashboard's issue log reads `Edited
  spec.md v4 to v7`. `DISPATCH_TEST_EDIT_NOTICE_QUIET`, under `DISPATCH_TEST_HOOKS=1`, sets the
  quiet time for an end-to-end test process. The actor is the sender of the latest edit a person
  made, never a name picked from the people an edit credits: an agent's edit after a
  person's still names the person, which the new column `doc_settlements_pending.last_person`
  (same migration) keeps across processes; an edit made while two peers were connected names
  nobody: an open window keeps its actor and takes the edit in, and with no window open the edit
  holds the window until a later version names a person, whose notice covers both.
- Each block of a Dispatch document is a node of the reference graph. A reference can name one
  block, `dispatch://KEY/spec#b-<id>`, `dispatch://KEY/artifact/<slug>#b-<id>` or
  `dispatch://PROJECT/artifact/<slug>#b-<id>`, the id percent-encoded as `text.EncodeBlockID` and
  `encodeBlockId` write it, and so does a live document page's `#b-<id>`; `@v<N>` after the id
  pins it to version N of the document. N is at most 9223372036854775807: a larger N is no
  reference, `400 INVALID_REFERENCE` where the API is given one, and a pin past every version the
  document has is `404 NOT_FOUND` `no version N of <document>`. A block id of any length is
  written, by a seeded spec, an upload, an `insert`, a rebuild, an accepted suggestion or a
  browser's edit; the graph keys a block by its id, so one longer than 256 bytes, decoded
  (`text.MaxBlockIDBytes`), is no graph node: its version is written, a mention inside it starts
  at the block holding it, the server logs a warning naming the document, the longest id's length
  and the bound, and a read of its reference is `404 NOT_FOUND`. An ask block, a comment anchored
  in a block and an ask anchored in one are written under a block id of any length, so a
  settlement never fails on one. Text cites a reference, a block's or any other, only while its
  block id is within that bound and the target id the graph stores its edge under
  (`text.TargetID`: an issue's key, a document's `<KEY or PROJECT>/<slug>`, a block's
  `<KEY or PROJECT>/<slug>#<block id>`, an ask's, a comment's or a message's id) holds at most
  2048 bytes (`text.MaxTargetBytes`), since a slug or an item id has no bound of its own: past
  either the text is written and cites nothing, and a live document page whose block is past them
  names the document, one whose document or item is past them nothing. The bounds decide only
  what the graph indexes: `GET /api/v1/references?to=|from=` and `GET /api/v1/block-links` read
  the reference they are given whatever its length (`text.ParseRef`), so a node of any name is
  read, and no reference's length answers `400`. A new artifact's name, and the reference key
  `<owner>/<slug>` it gives the artifact, hold at most 2692 bytes: an upload past either is
  `400 INVALID_ARTIFACT` naming the bound, and an artifact already stored past it keeps taking
  versions and edits. A
  mention written inside a block starts at the innermost block holding it: `GET /api/v1/references`
  names that block as the source (`kind: "block"`, with
  `block_id`, `document`, `changed_version` and `changed_at`, the version its text last changed
  in), a node cited from two blocks of one document counts twice, a document's own read names the
  block at each edge as `block`, and each block is `part_of` its document, which a document's read
  lists only under `?kind=part_of`. A block reference's read carries the block's whole current
  `text`; a pinned one also carries `pin`, the block's text at that version and whether it is
  `unchanged`, `changed` or `removed` since. Deleting a block takes the links to it out of every
  list, count, closure and outgoing list until a version restores its id.
  `GET /api/v1/block-links?from=<document>&to=<document>` lists the links from the blocks of one
  document to the blocks of another. `GET /api/v1/artifacts/{id}/references` keeps every source.
  Migrations `0121`-`0126` add the block tables, the triggers that owe the index what any build
  writes, and the seed of the comments, asks and issue messages already holding `#b-`, and
  `0127`-`0128` rebuild the ask-block and block-anchor indexes over `block_id_key`, the id's
  SHA-256, holding `asks` while its two indexes build and `comments` while its one does; the
  server's block-index reconciliation (lease `block-index`, logged as `dispatch: block index`)
  indexes every document and text owed, with no command run, and takes one more lease connection
  per server, nine. `rebuild-refs` marks every document owed that index and prints `blocks_owed=`.
  The dashboard titles a block link with the block's text, its hover card shows the document and,
  for a pinned link, the version and how the block stands since, a pinned link opens that version,
  and Referenced by shows one row per citing block, each linked to its block; on a document's page
  each row also links the document's own block the edge meets (`block <id>`), so a source citing
  two of its blocks is two rows. The dashboard draws a picture and opens a link an author names
  whatever the length of its target's key; only bare text it links is held to the index's
  bounds. A search hit for an ask links its typed block (`#b-<id>`) only while that block is a
  graph node, and otherwise `?ask=<id>`, so `dispatch read` of the hit reads the ask.
- Every reference-graph node now carries `made_by`, `made_at`, `changed_by`, `changed_at`, `parent`
  and `parent_version`, and `dispatch read` prints them on a `Node:` line. They are on every node
  `GET /api/v1/references` and `GET /api/v1/block-links` answer, whatever its kind, always
  present: who made the node and when, who last changed its own text or fields and when (its
  maker while nothing has changed it since; a claim, an answer or a resolution is no change), the
  node it belongs to (`{kind, id, ref?}`) and the version of that parent it belongs to. A fact
  Dispatch never recorded is `[]` for an actor list and `null` otherwise: a session has none, and
  a block's maker is known only when the block is in its document's first version or Dispatch
  indexed the version before the one that first held it. Migrations `0130` and `0131` add
  `comments.written_version` and `asks.written_version`, the version current when a comment or
  an ask was written on a document without an anchor (an approval request's, the version it names
  when it opens), which a `before insert` trigger records for every build's insert, and `0133` and
  `0134` fill them on the rows stored before, from the latest version created at or before each
  item or, for an approval request, from its `ask.opened` event, holding `comments` and then
  `asks` `EXCLUSIVE` while they write, which every write of that table waits behind. Migration
  `0132` creates `component_facts`, which each import keeps from this release on: a component's
  `made_at` and `changed_at`, judged by a digest of its own fields. A component stored before it
  has no `made_at`, and no `changed_at` until an import changes it.
- `envoy-infra:gcp:Dispatch` takes `pprofPort`, which sets `DISPATCH_PPROF_PORT` on the app
  Deployment's `envoy-dispatch` container alone, so a deployment turns on Dispatch's loopback-only
  profiler through the component rather than by editing the Deployment, which the next update
  would revert. Unset, nothing is rendered and the pod is the one it always was. The component
  refuses, naming the input, a value outside 1-65535 and the ports the app pod already binds on its
  loopback, `8766` and the `cloud-sql-proxy` sidecar's `5432`.
- `envoy-infra:gcp:Dispatch` takes `appCpu` and `appMemory`, the Kubernetes quantities that size
  the app Deployment's `envoy-dispatch` container as both its request and its limit: 1 CPU and
  `4Gi` by default, the size it always had, so a deployment that sets neither renders the same pod.
  The `cloud-sql-proxy` sidecar, the front door and the listener keep their sizes. The component
  refuses, naming the input, a value that is not a quantity or is empty, a decimal exponent
  outside `e-9` to `e18`, and a size at which GKE Autopilot would not run the app pod (the two
  containers' sum) as rendered: below `500m` or `512Mi` or outside 1 to 6.5 GiB of memory per
  CPU, which Autopilot raises, or above 30 CPU or `110Gi`, which it rejects. On a cluster without
  Pod bursting Autopilot also rounds the pod's CPU up to a multiple of 250m.
- `envoy-infra:gcp:Dispatch` takes `appPriorityClassName` and `appPdbMinAvailable`, which protect
  the app Deployment's pods from being displaced, so a deployment sets both in its program rather
  than by hand. `appPriorityClassName` is the app pod's `priorityClassName`, naming a PriorityClass
  that already exists in the cluster; the component reads the class and the app Deployment waits
  on the read, so a missing class fails the preview or update rather than leaving the one app pod
  stopped and its replacement refused. `appPdbMinAvailable` registers a PodDisruptionBudget named
  for the app, over its pods alone, with that `minAvailable` and Kubernetes' default
  `unhealthyPodEvictionPolicy`, so a drain waits for the app. The front door and the listener are
  unchanged, and with neither set the component renders what it always did. The component
  refuses, naming the input, an `appPriorityClassName` that is empty, not a DNS subdomain or in
  the reserved `system-` prefix, and an `appPdbMinAvailable` below 1 or above `replicas`.
- `envoy-infra:gcp:Dispatch` takes `proxyCpu` and `proxyMemory`, the Kubernetes quantities that
  size the app pod's `cloud-sql-proxy` sidecar as both its request and its limit, so a deployment
  sizes the proxy every database call of the pod passes through. They default to 1 CPU and `2Gi`
  (see Changed), and the component judges them with `appCpu` and `appMemory` against Autopilot's
  bounds for the pod's sum, refusing them as it refuses those, each refusal naming the inputs; a
  size that is not above 0 is now refused too.
- `envoy-infra:gcp:Dispatch` takes `frontDoorPdbMinAvailable` and `listenerPdbMinAvailable`, which
  register a PodDisruptionBudget named for the front door (`<hostname>-tailnet`) or the listener
  (`<hostname>-listener`), over that Deployment's pods alone, as `appPdbMinAvailable` does for the
  app, each refused below 1 or above its Deployment's replica count. A pod template whose
  Deployment has a budget no longer carries the cluster autoscaler's `safe-to-evict: "false"`
  annotation, since the budget bounds the autoscaler's evictions too; a template with no budget
  keeps it. A budget made by hand under one of those names must be deleted before the update that
  sets its input, since the component's provider does not adopt it.
- `envoy-infra:gcp:Dispatch` takes `frontDoorReplicas`, the front door's pod count, 2 by default,
  and `listenerReplicas`, the listener's, 1 by default, each refused below 1. The front door runs
  two by default, since with one pod every node drain, upgrade or scale-down that moves it takes
  Dispatch off the tailnet until its replacement is ready. Raise `listenerReplicas` above 1 only on
  an envoy image whose listener holds a delivery lease, where a standby pod is ready and serves
  `/v1` while only the lease holder delivers.
- `envoy-infra:gcp:Dispatch` takes `databaseAvailabilityType`, the Cloud SQL instance's
  availability type: `ZONAL`, the default and the instance's value until now, or `REGIONAL`, a
  standby in a second zone that Cloud SQL fails over to; anything else is refused by name.
  Switching changes the instance in place, since the bridged Terraform provider declares
  `settings.availability_type` without ForceNew: Cloud SQL restarts the instance once, and the
  change cannot be stopped once started. `REGIONAL` roughly doubles the instance's cost.
- `envoy-infra:gcp:Dispatch`'s front door runs `frontDoorReplicas` pods that host one Tailscale
  Service, `svc:<hostname>`, each pod its own tailnet node, registered under the pod's name with its
  state on an `emptyDir`, so `https://<hostname>.<tailnetDnsName>` and the SOCKS5 proxy the app and
  the listener reach NATS through answer while any one pod serves. A pod advertises the Service
  only through its advertiser, is ready only while it hosts the Service, and on stopping drains the
  Service, waits 20 s and logs its node out inside a 60 s grace period. The Service's certificate
  is kept in the Secret `<hostname>-tailnet-tls`, which a new pod seeds its cache from and the pods
  publish a later certificate to, and a pod with no certificate more than 31 days from its end takes
  one of 3 claims per 7 days recorded on that Secret before it may order one, under Let's Encrypt's
  5 per name per week. `cert-publish` runs on `frontDoorToolsImage`; see the Breaking entry under
  Changed for the move.
- Dispatch writes one access-log line per served request, so the callers behind a load spike can
  be named: `dispatch: request` at INFO when the request ends, with the method, the route's own
  pattern, the escaped request path, the caller (a person's email, a bearer's session, a personal
  token's owner, a verified service-account subject, or the shared token with no owner to name),
  the credential that named a session, the forwarded address and the user agent, the status, the
  milliseconds it took and the response bytes. It covers every `/api/v1` route, the document and
  delivery websockets, `/auth/*`, the GitHub proxy and the 404 of a path no route answers, still
  writes its line when a handler panics, names the caller a websocket or stream was admitted as
  however many heartbeats it lives, cuts every field a caller wrote to 256 bytes, and never
  carries a token, a cookie, a query string, a request body or a response body. It is always on:
  it costs about 2.6 µs and 12 allocations per request and makes no database call
  (`internal/dispatch/api/access_log_bench_test.go`), and the question it answers is asked when a
  deployment is already in trouble (`cmd/dispatch/README.md`, "Access log").
- A release of a `github-app` secret can be narrowed: `POST /v1/grants/{id}/values` takes an
  optional `github_app` body (`owner`, `repositories`, `read_only`) that mints the token of one
  account `github-owners` lists alone, sent to GitHub as the `access_tokens` body, limited to
  those repositories of the account (at most 100), to `contents: read` and `metadata: read`, or
  to both; a token limited to repositories alone keeps every permission the App holds. It can
  only narrow, applies to that release alone, and the answer names it as `github_app`, which the
  broker releases only once GitHub's own answer shows the token holds no more, revoking at GitHub
  a token whose answer holds more. A bad body, a `repositories` list present but empty among
  them, is `400 INVALID_NARROWING`, a narrowing of a grant holding no `github-app` secret
  `400 NARROWING_NOT_APPLICABLE`, one to an account its `github-owners` tag does not list
  `403 NARROWING_OWNER_NOT_LISTED`, and one GitHub refuses (a repository the installation does not
  reach) `422 NARROWING_REFUSED`; the last two are logged at WARN as `broker: narrowing refused`.
  `agent-secrets`' exec form takes `--github-owner`, `--github-repo` and `--github-read-only`,
  exits 2 on a narrowing the broker would refuse `INVALID_NARROWING` before it sends any request,
  and runs its command only once the broker names the narrowing it applied, so deploy the broker
  before any caller passes `--github-owner`.
- `envoy-dispatch` serves on an inherited listening TCP socket that `DISPATCH_LISTEN_FD` names by
  its descriptor number, and binds nothing: a test harness opens the socket on port 0 and holds the
  port the kernel chose until Dispatch serves on it, so no other program can take it in between.
  `DISPATCH_LISTEN_HOST` and `DISPATCH_PORT`, when set and not empty, must name the socket's
  address, and `DISPATCH_SERVER_URL` is not derived from it. A descriptor that is not a listening
  TCP socket, or a setting that names another address, refuses startup naming the variable, with
  nothing bound in its place, and `DISPATCH_DEV_SIGNIN=1` refuses a socket that is not on a
  loopback address. The `dispatch: listening` line names the address the server is bound to, in
  both modes: a server on every interface logs `addr=[::]:8766` where it logged `addr=:8766`.
- `envoy-listener` serves on an inherited listening TCP socket that `ENVOY_LISTEN_FD` names by its
  descriptor number, and binds nothing, for a process a test harness starts (not a container:
  Docker cannot pass a host descriptor in). `ENVOY_LISTEN_HOST` and `PORT`, when set, must name the
  socket's address, and `PORT=0` names none; a descriptor that is not a listening TCP socket, or a
  setting that names another address, refuses startup naming the variable, with nothing bound in
  its place. The API auth rule judges the socket's own address, so a socket on every interface
  needs `ENVOY_API_TOKEN`, the OIDC pair or `ENVOY_API_ALLOW_UNAUTHENTICATED=1` as
  `ENVOY_LISTEN_HOST=0.0.0.0` does, though an unset `ENVOY_LISTEN_HOST` means loopback. The
  `envoy-listener listening` line's `addr` is the address the listener is bound to, in both modes:
  `PORT=0` logs the port the kernel chose where it logged port 0, and a listener on every interface
  logs `[::]:9020` where it logged `0.0.0.0:9020`.
- Dispatch can serve the Go profiler: `DISPATCH_PPROF_PORT` set to a port serves
  `net/http/pprof` on `127.0.0.1:<port>` alone, on a mux of its own, so a CPU profile of a busy
  server is one `kubectl port-forward` and `go tool pprof` away. Unset or empty, nothing listens;
  a value that is not a port 1-65535 refuses startup naming the variable, the profiler binds
  before anything connects, and it closes at once on shutdown, delaying nothing. `DISPATCH_PORT`
  is read by the same rule, so a value past the integer range, which the old check accepted and
  only the bind refused (after NATS and the migrations had run), is refused at startup too.
- Dispatch's meaning search embeds through AWS Bedrock (the default, unchanged) or Azure AI
  Foundry, chosen by `DISPATCH_EMBEDDER`. Azure takes `DISPATCH_AZURE_EMBED_ENDPOINT`,
  `DISPATCH_AZURE_EMBED_TOKENS_PER_MINUTE` (the deployment's quota, two thirds of which paces
  background embedding), `DISPATCH_AZURE_EMBED_DEPLOYMENT` (default `embed-v-4-0`) and one
  credential: the deployment's key (`DISPATCH_AZURE_EMBED_KEY` or `_FILE`), or a keyless sign-in
  that exchanges a projected token with Microsoft Entra (`DISPATCH_AZURE_TENANT_ID`,
  `DISPATCH_AZURE_CLIENT_ID`, `DISPATCH_AZURE_FEDERATED_TOKEN_FILE`). Boot refuses, naming the
  setting, an unknown place, an Azure setting under Bedrock, and an Azure configuration missing its
  endpoint or quota or holding both credentials, neither or part of the keyless one; a model
  service or Entra that is down refuses no boot. Background embedding sends Azure at most 320,000
  bytes of text a call, so a call of dense text (CJK, hex) finishes well inside the 60-second
  request timeout instead of timing out and being split; Bedrock's batches are unchanged. Pacing
  estimates 4 characters a token, which under-counts such text, so a backlog of it can meet
  Azure's 429s, which the queue retries as throttles (INFRA-393).
- The Dispatch server keeps meaning search's coverage itself: a coverage pass at boot and every 10
  minutes embeds items written before the embedding triggers existed or with them off, repairs a
  row whose text changed with the triggers off, drops every vector another embedder made and
  re-admits a row that died under another embedder, with no lease, so every process may run it.
  It drops and re-admits only once the configured embedder has answered (an embedding by this
  process has succeeded, or a short canary text it embeds does); until then it leaves those rows in
  place and logs one WARN naming the identity, the canary's error and how many it left, so a
  misnamed Azure deployment or a refused credential drops no vector.
  `GET /api/v1/search/coverage` reports the configured embedder's identity, place and model, and
  in total and per kind the searchable items with a current vector, lacking one or failed, and the
  oldest lacking item's wait (INFRA-393).
- `agent-secrets` names the store it answered from, so no answer of its own is read as another
  secrets CLI's about the same name: the broker `AGENT_SECRETS_URL` names, and the kind of that
  broker's store, read once from its public `GET /v1/settings` (`AWS Secrets Manager`, `GCP Secret
  Manager`, or the store the broker names when this release does not know it), as `the broker
  AGENT_SECRETS_URL names (<store kind>)`, with no `(<store kind>)` where the settings cannot be
  read. The injection form writes one line to standard error before it execs -
  `agent-secrets: NAME, NAME from the broker AGENT_SECRETS_URL names (<store kind>)`, the names it
  injected and nothing of their values; `request`, `status` and `self` print a `source:` line;
  `machine list` and `grant list` name their broker the same way; and every refusal names where
  this CLI looked, from a denied or pending request to `UNKNOWN_SECRET` and a grant that released
  no value. None of these lines names the broker's host or the store's account, region or
  project, since an agent's output is quoted wherever it goes, a public pull request among them.
  The secret forms, which a person runs under their own cloud sign-in, name the store's account or
  project (`AWS Secrets Manager account <id> in <region>`, `GCP Secret Manager project <number>`):
  `secret show` prints a `store:` line and every row of `secret list` carries `store` in `--json`
  and a line on standard error naming the store the table is from. `--json` output is unchanged
  for `request`, `status`, `self`, `whoami`, `machine list` and `grant list`, which still print
  the broker's own body verbatim and make no settings call; `secret list --json` and
  `secret show --json` gain a `store` field. No line names a secret's version: the broker
  releases a grant's values as names and values alone, and a session holds no sign-in of its own
  to the store to read one with (`agent-secrets secret show`, which reads the store directly
  under the person's own sign-in, lists each version as before).
- A broker call that did not complete says what failed, rather than leaving a bare transport error
  to read as a broker refusal: the host does not resolve, name resolution failed, the connection
  was refused, no connection could be made (network or host unreachable, or any other dial error),
  nothing answered in time, or a peer answered and then broke the exchange (TLS, an HTTP answer to
  https, EOF or a reset after the request); any other error says only that the request failed. It
  names the method and URL it tried, and that `AGENT_SECRETS_URL` named that address, so a session
  that inherited a stale address reads a stale address. Such a message carries no `asked the
  broker …` trailer: the broker refused nothing. A local helper refusal or dead helper socket names
  that socket instead, since no broker was asked.
- No output of `agent-secrets`, `agent-secrets-helper` or `agent-secrets-devrelay` prints a
  credential the broker's URL carries. Every message that names the URL names it through
  `internal/redact`'s allow-list: the URL prints as written only when it is provably a plain
  http(s) URL - printable ASCII with no `@`, `%`, `?`, `#`, backslash or whitespace, an http or
  https scheme, a DNS name, IPv4 or bracketed IPv6 host with an optional numeric port, and a path
  of letters, digits and `._~/-` alone - and as `(not printed: the value carries a credential or is
  not a plain http(s) URL)` otherwise. No part of a value that fails the check prints, so no
  separator, escape (single or double) or control byte in it can show a credential or forge a
  line. For such a URL, the transport's own error prints as its class alone (`name resolution
  failed`, `connection refused`, `timed out`, `TLS failed`, `the broker answered HTTP <n>`,
  `not a valid URL`, `the request failed`), since its text can quote the URL; it still unwraps
  to the original, so every classification reads as before, and a clean URL's errors print as
  before. This covers a broker that could not be reached, the secret forms' refusal of a
  plain-http broker, the helper's `listening` log line, and every error the helper's broker
  calls make (a machine login, an enrollment, a renewal, a refresh or a revoke, which
  `agent-secrets machine login`, `register --wait` and the helper's log print), the helper's
  `sign-launcher` refusal of a URL that is not its broker's (which `machine` and `grant` list and
  revoke print), `agent-secrets-helper install`'s refusal of a URL a unit line would need
  escaped, Dispatch's two boot errors about `DISPATCH_AGENT_SECRETS_URL`, and the broker's own
  refusals of `BROKER_PUBLIC_URL` and `BROKER_MACHINE_LOGIN_URL`. With a clean base, an error
  prints as net/http wrote it only when the URL it names is that base followed by a plain `/v1/`
  path and the error it wraps is a transport failure (a dial, name resolution, refused, timeout,
  TLS, net/http's `http: server gave HTTP response to HTTPS client`, EOF, a reset or a broken
  pipe), an HTTP status answer, or text the allow-list passes: a redirect's target (another host
  or path, a query, or a malformed `Location` whose text quotes it) prints the class alone.
  `agent-secrets` refuses an id that would not name one path segment as written - `"req%zz" is
  not a request id`, with no `asked the broker` ending since nothing was sent - before any call,
  since the proof signs the path as built and the broker checks it against the path it decoded;
  before, such an id was blamed on the broker's address. An http server answering an https
  `AGENT_SECRETS_URL` now reads as `the broker at … answered but the exchange failed`, as the
  troubleshooting guide says: net/http reports it as its fixed `http.ErrSchemeMismatch`, which
  the classification did not look for.
  `agent-secrets-devrelay --broker` no longer takes `AGENT_SECRETS_URL` as its flag default,
  which its usage text printed whole; it reads the variable after parsing. `agent-secrets-helper
  install` writes its unit `0600`, not `0644`, since the unit's `Environment=` line holds the
  URL, and narrows a unit an older install left `0644`. Before, net/http masked a password alone
  and printed the user name and query, and the message about an unreachable broker printed the
  whole URL, twice since the entry above.
- The `ask.opened` or `ask.answered` Dispatch's settlement appends when an edit restores a decision
  block it retracted carries `restored: true`: the ask returns to the state it had, and the event
  records no new answer, so a reader that takes an `ask.answered` for a person's answer can tell
  the two apart. A restore the release before this one writes carries no mark - every one already
  in the event log, and one written during a rollout or an image rollback - so a missing mark does
  not prove a new answer.
- Dispatch's approval read (`approval` on every document read) names the latest review's event
  sequence, `seq`, beside its `at`: each review keeps the sequence of the `artifact.approved` or
  `artifact.changes_requested` event it appended on its document's owner
  (`artifact_reviews.event_seq`, migration 0120, written in the same transaction). The owner's row
  lock orders those events as they commit, so `seq` places a review among them where `at`, its
  transaction's start, cannot. Every read that picks a document's newest review now orders by that
  sequence (`docs.ReviewOrder`), so two reviews written at once read back in the order they
  committed. A review recorded before the migration reads with its `at` and no `seq`, and so does
  every read the release before it serves during a rollout or a rollback, so a missing `seq` does
  not prove an older review. A review the release before it writes while a rollout or a rollback
  runs both releases gets its sequence too, from the migration's trigger at its commit, so it reads
  as the newest review it is rather than ranking below every sequenced one. 0120 is additive (a
  nullable column, a `not valid` check, an index in the reads' order and a trigger), so rolling
  back past 0120 alone, to a build that already carries 0117, is an image rollback: the release
  before it keeps writing reviews, and the trigger sequences each one. 0117's own rule still
  forbids starting a release older than 0117. The index (`artifact_reviews_by_artifact_event_seq`)
  is built in the migration's own transaction under the table lock it already holds, reading every
  review once.
- Dispatch's conversation view (`/agents/<id>/live`) shows the model and thinking level an Oh My
  Pi session runs now, from the `settings` frame pi-envoy publishes, before any turn runs on them
  and within about 20 seconds of a change made anywhere else (INFRA-195). Where the session runs
  `/model` and `/effort` from Dispatch, each is a native picker in the header: the model picker
  lists every model the session can use, grouped by provider, and the thinking picker the levels
  `/effort` offers. On a focused picker the arrows and type-ahead only move its choice, `Enter`
  switches to it, and leaving the picker without `Enter` puts it back on the reported value, as
  the Agents issue picker's keyboard rule does. A pick is the person's own Send of
  `/model <provider/id>` or `/effort <level>`, always as Send and stored under their name; while
  it waits for the session's answer Send is disabled and the pickers are held, keeping focus, and
  the line under the header quotes the session's reply, or says no answer came within 30 seconds
  or that the message could not reach the session. The header
  changes only when the session reports the new value. A session on an older pi-envoy shows the
  last turn's model and asks to be restarted on a newer one; a session whose Oh My Pi cannot tell
  its settings, or that runs neither built-in from Dispatch, says so. No route changes: an agent's
  token is still refused on `POST /api/v1/agents/{id}/messages` (`403 HUMAN_ONLY`).
- A Dispatch project imports its component model from any number of architecture sources, one
  per repository, each on its own branch, and its model is every source's components together
  (INFRA-391). `GET /api/v1/projects/{key}/architecture-sources` lists a project's sources (`[]`
  with none); `PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}` `{branch}` adds a
  source, or changes the branch of the one it already is, after proving the GitHub App can read
  the repository and that `branch` is the name of a branch it has (an exact
  `git/ref/heads/{branch}` read): a missing branch, a commit SHA, a relative ref such as
  `main~1`, `HEAD` or a `refs/` path is `409 SOURCE_ACCESS` naming the value and the repository,
  as a credential problem is, and nothing is written;
  `DELETE` on the same path removes that source and its components, and no other source's;
  `POST …/architecture-sources/{owner}/{repo}/sync` imports one source and
  `POST /api/v1/projects/{key}/architecture-sources/sync` every source, each attempted whatever
  another's import did and each answering on its own row. A source row carries `components`, how
  many components it projects; `GET /api/v1/projects/{key}/architecture` carries `sources` and
  each component's `source` repository; `architecture.synced` and `architecture.sync_failed`
  name the source's `repo` and `branch`. One source's failed import keeps that source's previous
  components and touches no other source's. Component ids stay project-wide: an import whose
  model holds an id another source of the project already has fails for that source alone,
  naming the id and the repository that holds it, and an issue may name components of any
  source of its project. Settings lists one row per source with its component count and its own
  Refresh and Delete; the architecture page has a header line per source with its own Refresh,
  and hatches only the components of a source that failed or is stale; a component's details
  name its repository; an issue's component picker offers every source's components. Migration
  `0117` adds `architecture_sources.id` and `components.source_id`: every existing source and
  component reads `0`, so a project with one source keeps it, its components and every issue
  attachment, and nothing re-imports.
- A project's default assignee, the last step of an issue create's assignee chain: an issue created
  with no assignee, by nobody (the shared token), under no parent or under a parent with nobody on
  it, goes to the person the project names; nothing already filed, and no create that names an
  assignee or `null`, changes. A default that no longer names a person who has signed in is
  ignored rather than refused, so a create never fails on a stale one. A person sets it on the
  Settings page through
  `GET /api/v1/settings/project-default-assignees` and `PUT /api/v1/projects/{key}/default-assignee`
  `{assignee}` (an email of someone who has signed in, or `null` to clear it; otherwise
  `400 ASSIGNEE_NOT_ALLOWED`, and a body without `assignee`, or whose `assignee` is neither a
  string nor null, is `400 INVALID_PROJECT`), which appends
  `settings.project_default_assignee.updated`. Both routes are human-only and no agent command,
  flag or route sets a project's default.
- A release of a stored secret checks the policy again once it has read the store: a retag to a
  kind (`github-app` or `google-login`) the broker rereads while the release reads the secret
  releases nothing and ends the grant for good, as a kind change found before the read does, so a
  grant made while the secret was plain never returns the App key or refresh token stored once it
  is a kind. Retag a stored secret to a kind, and store its key only after the retag exits 0,
  printing `broker: serving NAME (kind <kind>)`.
- A mint error names an upstream's HTTP status by its number and a failed call by its class
  (cancelled, timed out, not resolved, not reached, a failed TLS handshake, an answer ended early,
  or no usable HTTP answer), never Go's transport text, the reason phrase or the body, any of which
  an upstream can fill with the credential it was sent. A Google refusal names its `error_subtype`
  only when it is `invalid_rapt` or `rapt_required`, and `unknown` otherwise. A GitHub App
  installation record with no positive id is skipped: no token is asked for installation 0.
- `agent-secrets secret retag` writes against the tags it checked. On GCP it sends the annotations
  and etag of its one read, so a write another person made in between refuses it, `another write
  changed the secret after this retag read it`, with nothing written. On AWS, whose tag calls take
  no condition, it reads the tags back after its write, and when another writer left a `github-app`
  secret without owners it restores the tags it read and exits 1 naming the race: retag on AWS one
  writer at a time.
- `POST /v1/secrets/{name}/reread` answers `kind`, the kind the broker serves the secret as (absent
  for a stored secret), and `agent-secrets secret retag` exits 1 when that is not the kind it wrote:
  `the broker still sees the old tags`, since a stale metadata read serves the secret as it was, and
  a secret served as stored releases its stored value. Store an App key or a login in a secret only
  after its retag to that kind exits 0, printing `broker: serving NAME (kind <kind>)`.
- A person's machine login is an OAuth 2.0 device authorization grant (RFC 8628) on the secrets
  broker's own `POST /v1/oauth/device_authorization` and `POST /v1/oauth/token`, and it refreshes
  itself. `agent-secrets machine login` prints the same code, which the person the login names
  types into Dispatch's machine-login page as before; on approval the helper gets a refresh token
  bound to the machine's key (DPoP, RFC 9449), and refreshes the login when it starts and then at
  half the time left before the credential's expiry, at most every 24 hours; a refresh whose
  answer is lost, that does not reach the broker, or that the broker answers 5xx is retried every
  15 s until the broker settles a refresh (a success or `invalid_grant`), answered refusals in
  between included; otherwise a 429 waits its `Retry-After`, at least 15 s and at most a minute,
  and any other refusal but `invalid_grant` a minute, doubling to an hour; an outage longer than
  five minutes (the time a rotated token stays redeemable) right after a lost answer makes the
  next retry a reuse. A token answer naming no new refresh token is not saved: the helper keeps
  the token it presented and retries. Both
  OAuth routes refuse a form past 64 KiB, `400 invalid_request`. `machine login` names the page the broker's `verification_uri`
  names when `AGENT_SECRETS_APPROVE_URL` is unset. Each refresh moves the credential's expiry to `BROKER_LAUNCHER_CREDENTIAL_SECONDS`
  from then and rotates the refresh token. A rotated token redeemed again within five minutes of
  its replacement ends the tokens its earlier redemption issued, so the login never forks; one
  redeemed later is a reuse, which ends the login and revokes the machine's launcher credential
  as a revoke does (its sessions and grants ended, audit actor `broker:refresh-token-reuse`), so
  a copied machine file's key enrolls nothing more. The token endpoint checks each DPoP proof
  against `BROKER_PUBLIC_URL`, never the request's `Host` or `X-Forwarded-*` headers, accepts a
  proof within `BROKER_PROOF_SKEW_SECONDS` of the broker's clock, and spends a per-address rate
  limit bucket of its own (429 `RATE_LIMITED` with `Retry-After`). A login through the earlier
  `POST /v1/launcher-credentials` route, which cannot refresh, keeps its seven-day credential, or
  `BROKER_LAUNCHER_CREDENTIAL_SECONDS` when that is shorter (that route is removed since: see the
  Breaking entry under Changed on serving a person's machine login only through the OAuth device
  grant). The provider's device codes, tokens,
  DPoP proofs and nonces and its HMAC secret live in the tables migration 0016 creates. Revoking a machine (Dispatch or `agent-secrets machine revoke`) ends
  its refresh tokens too, so its helper's next refresh is refused, and the helper drops the login,
  logs `the broker refused the machine login's refresh: it was revoked, lapsed, or its refresh
  token was reused` at ERROR, and `machine login-status` prints it. While a login is held,
  `machine login-status`'s stderr says `the machine login renews itself; without a refresh it
  lapses at <time> (in <duration>)`. Only an ended login answers a refresh `invalid_grant`; a
  refresh the broker cannot serve (its database unreachable) answers `500 server_error`, which
  the helper retries. The machine-login page takes the code as a person types it, in lower case
  or with a space for the dash.
- The helper keeps its machine login in `AGENT_SECRETS_MACHINE_FILE` (default
  `~/.config/agent-secrets/machine.json`, mode 0600): the machine key, the credential's id and
  expiry and the current refresh token, written through a fresh temporary file so no file or link
  already in the directory is followed. A file its group or anyone else can access, or another
  user owns, stops the helper at start, naming it and `chmod 600`; temporary files an interrupted
  save left beside it are removed at start. It refreshes that login before it resumes it at start,
  waiting at most 5 s so its socket listens within `register`'s wait, so
  a restart, a reboot or an upgrade keeps the machine logged in. A machine revoked while its
  helper was stopped is refused by the broker from then on, and the helper drops the login at its
  first refresh or launcher call the broker refuses. It deletes the file when the broker refuses
  the login's refresh, within seconds of a
  launcher call the broker refuses `LAUNCHER_INVALID`. A refresh the broker rate-limits (429)
  keeps the login and is retried no sooner than its `Retry-After` (at most a minute). A refreshed
  login it cannot save is held in memory, the machine file, which names a token the broker
  rotated away, is removed at once, and only its save is retried, every 15 s, with no further
  token request on any path; a restart before the next good save holds no login and needs a new
  approval rather than presenting that token past its grace, which the broker would revoke as a
  reuse; a later save writes the file again. A read-only filesystem that refuses even the removal
  is logged at ERROR: a restart before it is writable again revokes the machine. A machine login
  that replaces a credential the helper held, or follows one a `LAUNCHER_INVALID` dropped, retires
  the old one at the broker, best effort, through the new
  `POST /v1/operator/machines/{id}/retire`, signed with that credential's own launcher proof and
  accepted only for the calling credential: the old login can no longer refresh or enroll a
  session, and the sessions it enrolled keep running, so a new login leaves the machine's running
  sessions and their grants alone. `serve` holds exclusive locks on
  `<socket>.lock` and `<machine file>.lock` for its lifetime and refuses to start, naming the
  socket or the machine file, while another helper holds either, before it spends any refresh
  token; it refuses a path at the socket that is not a
  unix socket. A file that is there but is not a machine login stops the helper at start,
  naming the file.
- `agent-secrets-helper install [--url URL] [--restart-with-sessions]` writes the helper's
  `systemd --user` unit (`$XDG_CONFIG_HOME/systemd/user/agent-secrets-helper.service`, by default
  under `~/.config`), running this binary's `serve` with `AGENT_SECRETS_URL` from `--url` or the
  environment, each of `AGENT_SECRETS_HELPER_SOCK`, `AGENT_SECRETS_OPERATOR_FILE` and
  `AGENT_SECRETS_MACHINE_FILE` that is set in install's environment, and `Restart=always`, then runs `systemctl --user daemon-reload` and
  `enable --now`; it refuses a relative path in any of those three variables, naming it, since the
  unit's helper runs in another directory. An unchanged unit that is running changes nothing; one that is not is started.
  A helper answering on the socket that the unit does not run is refused, even when the unit file
  already matches: install lists its sessions, says to stop it, exits 2, and writes and starts
  nothing. A helper that was already running is restarted after a unit change
  only when it has no sessions registered, or with `--restart-with-sessions`, which restarts it
  even when the unit is unchanged, since a restart ends
  the sessions' grants; with sessions it lists them, prints `not restarted: <n> sessions are
  registered; a restart ends their grants. Re-run with --restart-with-sessions.`, and exits 0. The
  helper's new `drain` op is that check: it answers the registered sessions and, with none, refuses
  every later register `DRAINING` until the helper exits, so no session starts between the check
  and the restart; a restart that fails sends the new `undrain` op, so the helper takes sessions
  again. A running helper from before `drain` is never restarted without
  `--restart-with-sessions`: install lists its sessions and says to re-run with that flag at a
  quiet moment. `agent-secrets register` retries a `DRAINING` answer within its `--wait` (at least
  10 s), and `agent-secrets enroll --helper` within 30 s, so a session or box launched during the
  restart registers or enrolls with the restarted helper.
- The secrets broker's sweeper deletes spent machine-login OAuth state each tick: device codes an
  hour past their expiry, expired DPoP proofs, nonces and access tokens, and refresh token
  families none of whose tokens can still be redeemed, at most 10,000 rows of each per tick.
- Each `ghcr.io/sjawhar/legion/envoy:<commit>` image published from now on carries a GitHub
  artifact attestation of its index digest, signed by `release-envoy-listener.yaml`, so `gh
  attestation verify --repo sjawhar/legion` with `--cert-identity
  …/release-envoy-listener.yaml@refs/heads/main --source-ref refs/heads/main
  --deny-self-hosted-runners` accepts an image only when a run on `main` built it. `latest` is a
  separate index with no attestation; pin the `<commit>` image its revision annotation names.
- The secrets broker mints releases by each secret's `kind` tag. `github-app` releases contain
  installation tokens for every installation of the GitHub App that is not suspended and is on an
  account the secret's `github-owners` tag lists (GitHub logins separated by spaces); a
  `github-app` secret without that tag is refused `github-owners-missing`, and one whose list
  holds a comma, or an entry no GitHub login can be (not starting with an ASCII letter or digit,
  holding anything but ASCII letters, digits and hyphens, over 39 characters, or the word `none`),
  `github-owners-malformed`,
  since anyone can install a public App. `google-login`
  releases contain
  a Google access token. A kind secret holds its key or refresh
  token as a current value, which the broker never returns, and a mint GitHub or Google refuses
  is `502 MINT_FAILED` with their reason, logged at ERROR as `broker: mint failed` with the
  secret's name, kind and the failure's class. `max-grant` limits a request's `--ttl`; a missing or
  longer lifetime is `400 GRANT_REQUIRES_LIFETIME`. A grant ends for good when a release finds its
  secret's kind changed or its lifetime over the current `max-grant` (`GRANT_NOT_LIVE`); a change
  reverted before any release finds it leaves the grant as it was, and to end a grant, revoke it. A
  `max-grant` set or lowered later also refuses approval of a longer pending request
  (`NOT_APPROVER`). `BROKER_MINT_TARGETS` is removed and the broker refuses it when it is set.
  `agent-secrets secret create` takes `--kind`, `--github-owners` and `--max-grant`, and `secret
  retag` takes `--kind`, `--github-owners LOGINS|none` and `--max-grant DURATION|none`. A retag
  either sets tags or removes them (`none`), in one write, so a refused write leaves the secret as
  it was. A retag that names `none` beside another change exits 2 before it writes anything, and
  so does a create or retag that would leave a `github-app` secret without `github-owners`.

- Dispatch records which repository each issue targets. A repository mapping
  (`/api/v1/settings/repo-projects`) carries `default`, at most one per project, set from the
  Settings page's Default radio; an issue carries its own `repo`, which must be a mapping of its
  project (`400 REPO_NOT_MAPPED`), set from the issue page's repository picker; and every issue
  read and issue event carries `repo` and `resolved_repo`: its own, else its nearest ancestor's,
  else the project's default, else null. A mapping issues name is neither deleted nor moved
  (`409 REPO_PROJECT_IN_USE`), and a moved mapping emits `settings.repo_project.updated` on the
  project it left as well as the one it joined. Every mapping write, the `DISPATCH_REPO_PROJECTS`
  boot seed included, takes one set of locks in one order, so concurrent writes neither lose an
  event nor fail, and the seed now announces each mapping it adds. Migration `0113` adds
  `repo_projects.is_default` and `issues.repo`.
- An issue sits at most 32 links below its root: a create under a parent, or a reparent whose
  subtree's deepest issue would sit deeper, is `409 PARENT_INPUT`. Creating an issue with a parent
  now takes the project's dependency lock, as a reparent does, so two writes cannot together pass
  the limit. An earlier release could build a deeper chain, so migration `0113` refuses to apply
  while any issue sits more than 32 links below its root, or in a parent loop: its census counts
  them, so the pre-deploy census refuses first, and the migration, holding `repo_projects` and
  `issues` from before its count to its commit, names up to 20 of them and the repair, `PATCH` an
  ancestor with `{"parent": null}` on the release still running.
- The secrets broker reads its agent secrets from Google Cloud Secret Manager as well as AWS
  Secrets Manager. `BROKER_SECRETS_STORE` picks the store (`aws-secrets-manager`, the default when
  unset, or `gcp-secret-manager`); with the GCP store `BROKER_SECRETS_PREFIX` is
  `projects/<project number>/secrets/<id prefix>-`, `BROKER_SECRETS_KMS_KEY_NAME` names the
  agent-secrets Cloud KMS key in place of `BROKER_SECRETS_KMS_KEY_ARN`, and a secret's owner and
  tier are its annotations. The broker signs in under Application Default Credentials (on GKE,
  Workload Identity for its ServiceAccount; no key file) and needs only `roles/secretmanager.viewer`
  on the project and `roles/secretmanager.secretAccessor` on the namespace's secrets. `GET
  /v1/settings` names the store (`store`) and answers each store's own fields (`kms_key_name` and
  `gcp_project` for GCP), the other store's omitted. `secrets.Store` is the one interface the
  policy and the value release read through, with `secrets.AWS` and `secrets.GCP` behind it;
  `policy.Secret.ARN` is `Source`, the same bytes in the policy version's digest.
  `agent-secrets secret` manages both stores: against a GCP broker it reads and writes Secret
  Manager itself under the person's own Application Default Credentials, refusing a service
  account's.
- `ENVOY_NATS_SOCKS5_PROXY` sends every NATS connection of the bus through a SOCKS5 proxy, as
  `host:port`: the first, nats.go's reconnects in place, and the replacement a client's recovery
  dials, for `envoy-listener`, `envoy-dispatch` (a row of its settings table), `natstail` and the
  MCP bridge. The proxy is handed each server's host name as the URL spells it, unresolved, so a
  proxy that resolves names this process cannot (a userspace Tailscale container's SOCKS5 server
  and MagicDNS) reaches them. A value that is not `host:port` (whitespace around it, a scheme, an
  IPv6 address with a zone, a host that is neither an IP address nor a DNS name, a port outside
  1-65535, or empty) is refused before anything dials, and so is one that holds an nkey seed as
  `bus.HoldsSeed` finds one (the value, its host, a label or part of one, or split across labels by
  `.`, `-` or `_`, in either case), which is a valid DNS label but never a proxy address. A set
  proxy is remote reach, refused without `ENVOY_ALLOW_REMOTE_NATS=1`, since
  the proxy chooses the server. No refusal prints the value. A connect through the proxy that
  fails, its attempts spent or its deadline reached, names the setting and the proxy's address,
  with the proxy's own dial error where nats.go alone would report only `nats: no servers
  available for connection`, and a proxied dial ends at a connect's deadline as a direct one does.
  Unset, the bus dials NATS directly, as before.
- Dispatch signs people in with Google directly for one Google Workspace domain, with no Cognito
  sign-in pool and no AWS account: `DISPATCH_SIGNIN_DOMAIN` takes the place of
  `DISPATCH_SIGNIN_GROUP` beside the issuer, client ID and client secret, under the issuer
  `https://accounts.google.com` and a Google web client. A person signs in when their Google ID
  token's `hd` is the domain and Google has verified their `email`, and is named by that email;
  anyone else reaches the refusal page with the reason (not in the domain, email not verified, no
  email). The authorization request asks Google for offline access with `prompt=consent` and the
  domain as `hd`, and the hourly confirmation applies the same rule to the refreshed token. The code
  flow reads `email`, `email_verified` and `hd` from the verified token in domain mode alone; the
  shared verifier, which Cognito tokens, pods' tokens and the listener go through, is unchanged.
  Migration `0112` adds the nullable `people.sub`: in domain mode a sign-in records the Google
  account's `sub`, and a sign-in under another account for the same address (a Workspace admin
  gave a deleted account's address to a new one) ends every session of the address in the
  transaction that updates the locked person row, as does a confirmation whose refreshed token
  names another account.
  Exactly one of the group and the domain is set: both, a domain under another issuer, a group
  under Google's, and a domain that is not a lowercase DNS name refuse the boot, and header identity
  and dev sign-in refuse the domain. A deployment with the four Cognito settings boots, signs in and
  confirms as before; its refusal texts for partial settings now name the domain beside the group.
  The compose deployment passes `DISPATCH_SIGNIN_DOMAIN` through.
- Every live session now carries `host` beside `machine_id`: where the session process actually
  runs, as its own plugin reported it (envoy-client's `machineID()`), rather than whichever
  listener happened to register it. Sessions on separate machines registering through one shared
  listener previously all showed that listener's own `machine_id` as their machine. `machine_id`
  still names the listener that owns the session's delivery (`store.Registry.Match`), the only
  listener that delivers to it (the next entry), and `host` is where that listener reaches a
  session on a serving port, unless the session runs on that listener's machine and it has a host
  bridge.
  `host` is carried on `POST
  /v1/interests/subscribe` (`subscribeBody.Host`, every subscribe and heartbeat), stored on the
  session entry (`session.SessionEntry.Host`), and returned from `GET /v1/sessions` and `GET
  /v1/roles/<role>`, falling back to `machine_id` (`SessionEntry.HostOrMachine`) for a session
  whose plugin predates the field. `GET /api/v1/agents` carries it through unchanged, with its
  own defensive fallback to `machine_id` for a listener that predates the field too.
- A session on a serving port (the OpenCode plugin's way of being reached) receives a topic the
  stream does not store, such as a team topic, when its machine's listener runs with
  `ENVOY_SUBSCRIBE_ALL_TOPICS=1` (INFRA-122). Such a listener opens a second NATS client holding
  one core queue subscription on `notifications.>`, in the group
  `envoy-listener-live-<machine id, spaces as hyphens>` (`bus.ListenerLiveQueue`, so two listeners
  of one machine deliver each message once); it leaves a subject under `notifications.role.` to the
  role lane and one the stream stores to the durable, and fans every other notification topic out
  to the sessions on its machine subscribed to it, by the same `POST
  /session/<id>/prompt_async`. Such a topic is live: it reaches whoever is subscribed when it is
  published, once, and is never retained or retried. Unset, empty or `0` leaves it off, as before;
  any other value refuses startup naming the variable. Only a NATS user whose grant allows a
  `notifications.>` subscribe (`HostPermissions`, a host's own user) may opt in: under the hub's
  or Dispatch's listener grant the start ends with exit 1 and an ERROR naming
  `ENVOY_SUBSCRIBE_ALL_TOPICS`. Both compose files set it. `/healthz` gains `live_topics`,
  `subscribed` or `not_applicable`, and answers `503 {"status":"unhealthy","error":"live
  subscription inactive"}` when the opt-in is on and the live subscription is gone or the server
  refuses it (one restarted without the grant refuses the subscription the client re-sends, which
  nats.go keeps valid, so `bus.Client.CoreSubOK` reads the server's refusal until the subscription
  is sent again). Shutdown
  drains the live client for up to 3 s between the HTTP stop and the main client's drain: 10 s +
  3 s + 13 s = 26 s, inside every runtime's 30 s. envoy-infra renders it from
  `core.ListenerConfig.SubscribeAllTopics`, set by the optional input `subscribeAllTopics` on
  `host:Listener` and `aws:WebhookListener` and by `envoy-infra render webhook-listener-env
  --subscribe-all-topics`; left unset, every existing rendering is unchanged.

  Only the owning listener delivers to a session on a serving port. Its owner is the listener its
  stored subscription names (the interest's `machine_id`, the one `store.Registry.Match` fans its
  topics out on, which a re-registration through another listener keeps), else, with no
  subscription stored, the listener it registered through (the session entry's `machine_id`); a
  session neither names is every listener's. The owner reaches a session on its own machine at
  `ENVOY_HOST_BRIDGE` when the bridge is set: one registered through it, or one whose recorded
  `host` equals the owner's `ENVOY_MACHINE_ID` (compared without letter case), which a session
  that stayed on the owner's machine and re-registered through another listener records.
  Otherwise it reaches the session at the registration's recorded `host` when it is not empty,
  otherwise at `host.docker.internal` for one registered through it, else at that listener's
  machine id; a session that re-registered through another listener and records another host
  runs on that machine, so the owner never uses its own bridge for it
  (`session.Deliverer.hostFor`, an IPv6 literal bracketed). A listener without a bridge used to
  dial `host.docker.internal` for every
  session on its machine, whatever host it recorded, so a serving port bound to loopback alone was
  never reached and every delivery to it failed. Another listener's session is now that listener's
  to deliver: this one requests nothing and leaves an inbox message to the owner silently
  (`session.ErrWrongMachine`, `HandleAgentResult.OwnedElsewhere`: no delivery, no `no_holder`
  exception, no NAK, one DEBUG line), where it used to dial the session at the owning listener's
  machine id. Every listener on the bus reads each inbox message from its own durable, so a second
  listener that could reach the session would deliver the same message again; the owner always
  has the message. A session that re-registers through another listener stays with the one its
  stored subscription names, which matches its topics and delivers them. A session owned by a
  listener that cannot reach its port is reached by nobody.
  Since the recorded host now goes into the delivery URL, `POST
  /v1/interests/subscribe` with `port` above 0 refuses a non-empty `host` that is neither an IP
  literal nor a plain host name (letters, digits, `_`, `.`, `-`; `session.ValidHost`) with
  `400 {"error":"host is not a host name or address","expected":["host"]}` and writes nothing. A
  registration with no `host` (an older plugin) or no port is accepted as before.
- `deploy/compose/single-node.compose.yml` runs Envoy on one machine: a JetStream NATS of its own
  (its data on the `nats-data` volume) and the listener, both on `127.0.0.1` only, with
  `deploy/compose/single-node.env.example` as its settings template. The site's Envoy page ("Running
  Envoy on one machine") walks through starting it and pointing a machine's sessions at it.
- A human's answer to a decision block carries his approval forward (LEGION-573). When the
  version his answer writes differs from the version he approved only in its ask blocks, every
  block it holds that is not as the approved version held it is one he answered, and at least one
  is, Dispatch records an `approved` review in his name pinned to that version and appends
  `artifact.approved`, in the transaction that writes the version (`docs.carryApproval`, from
  `writeVersionTx` after the open request's move), rather than leaving his approval stale and
  asking him to approve again what he just decided. The carried review's `reason` is `Carried
  forward from the approval of version <N>: the answer to a decision block was the whole change
  since it.`; an approval request open at the carry is answered `Approve` in his name
  (`ask.answered`) and the review's `ask_id` names it, null otherwise. The new version is compared
  with the one he approved alone: a block of it not as that version held it must be his answer,
  given after that approval, one must differ, and no version between them is read. Who answered
  what is the document's asks rows, never the blocks' own attributes, which a peer on the document
  socket writes as it likes: an ask block no row of the document backs (forged, or not yet
  indexed), a block id the version repeats, a block whose server attributes are not what
  settlement's own writer leaves from its row (`state`, `answered_by`, `answered_at`, `selected`,
  `answer`, a copy's `copied_from`, an unreadable block's `invalid`; an answer settlement withheld
  for room is as the row has it), and the block of an answered or resolved row that asks other
  wording than the row was decided on (the wording settlement writes back) or that settlement
  cannot parse refuses the carry; an open row's body is free. The comparison is
  `docs.CutDecisionBlocks`, the markdown rendered again with every ask block left out wherever it
  stands, on both sides. An edit outside the blocks, a block he did not answer (open, resolved, or
  answered by someone else, whether added since his approval or held open by the approved version
  and answered or resolved since; a block answered before his approval and untouched since is
  content he approved), a block of the approved version the agent removed, an agent's reword of an
  answered block or a selection forged on one landing in the version his answer writes, an
  answered block the agent deleted and inserted again (its row stays answered; nothing of his
  changed, whether the approved version held the block or not), two people's answers in one
  version, an approval by someone else only, his own later request for changes or anyone's newer
  one, a copy's document when its source is answered, and a copy planted in the document since his
  approval carry nothing, as before; should two people's approvals both pass, nothing is carried.
  An approval request on a document a human approved earlier names that approval in
  `ask.approval.approved` (`{version, at, by}`), so the human reads what he is approving since the
  version he approved; a first request carries no `approved` key.
- A finished Actions job on the default branch, of a run with no pull request, is published on
  `notifications.github.<owner>.<repo>.job.<run_id>.completed` (LEGION-690): a completed
  `check_run` that lists no pull request, from the GitHub Actions App, whose
  `check_suite.head_branch` is the repository's `default_branch` and whose `details_url` names
  `/actions/runs/<run_id>/job/<job_id>`. The payload is `kind: "job"`, `repo`, `run_id`, `job_id`,
  `name`, `status`, `conclusion`, `started_at`, `completed_at`, `head_sha` and `url`, each text
  capped like every envelope's. A check run that lists a pull request still goes only to the CI
  store, a merge queue's or another branch's job is published nowhere, and `job` joins
  `contracts.GithubTopicKinds`, so the listener's subscribe warnings take it as a kind.

- The Delivery and Pipeline pages update themselves live (LEGION-690). `GET /ws/delivery` (any
  signed-in person or agent token) takes a `subscribe` frame naming a view, the GET route's query
  string and a preset or custom window, and answers a `snapshot`, the matching GET route's body
  with a revision per key; after that it sends only `delta` frames carrying what changed (pull
  requests, runs and session groups by id, map entries by key, counts by facet and value, the
  other fields whole), a `freshness` frame when the freshness verdict changes, and a `heartbeat`
  every 15 s, the only frame on a quiet window. Nothing re-reads on a timer, a preset's end stays
  at the present, and a reconnecting page resumes with its last change and revisions and is sent
  what changed since. A jobs read marks a run complete only when that same GitHub listing has every
  job finished, and Dispatch shutdown waits for delivery sockets and the live hub before it closes
  its database pool. Under `DISPATCH_TEST_HOOKS=1`, `POST /api/v1/delivery/_test/disconnect`
  closes every such socket. `packages/dispatch/e2e/delivery-live-probe.ts` watches the route under
  a bearer, one JSON line per frame, and compares what it applied with the GET routes.

- Dispatch has a Replies page (LEGION-687): every thread a person wrote in - an ask's thread, a
  comment thread, an issue message thread - where someone else has written since they last read
  it, newest reply first, with a `Replies N` count in the sidebar and, below 1280 px, the compact
  header. A reply, a message, or an answer someone else gives to an ask counts; the row names who
  wrote it, and an answer reads as `answered` with its chosen options and text. Reading the thread
  on screen, replying in it, or **Mark read** clears the row, and a later write brings it back; an
  open ask the Inbox lists stays off the page, and the Inbox is unchanged. Migrations 0095-0099
  add `user_threads`, kept by triggers on `comments`, `asks` and `messages`, seed it from history
  with every thread read, so the deploy marks nothing unread, and catch up any write that landed
  while the seed ran. `GET /api/v1/me/replies` lists the threads and
  `PUT /api/v1/me/threads/{id}/read` moves one forward, appending `user_thread_state.updated`,
  which never notifies. Opening a link to a comment (`/issues/<KEY>/comments/<id>`) now opens its
  thread.
- A table's entry in `GET /api/v1/artifacts/{id}/blocks` (and the issue- and project-slug forms)
  lists its rows and cells as `table.rows`: each row's block id and index (the index `delete_row`
  takes, 0 the header) and each cell's block id, row, column (its index in its row, the index
  `delete_column` takes, so a cell spanning columns or rows counts once), the opening words of the
  header drawn above it, and its own. They are placed by the same walk of the same live tree as
  `GET /api/v1/artifacts/{id}/blocks/{block_id}`, so the two routes agree for every row and cell,
  and the list still holds one entry per table. The document's tables are laid out in document
  order on the one budget of span cells a render of it spends, so a cell's header is the one `GET
  .../text` draws above it, and the block route places a cell under that same header; a table past
  the budget is listed as its markdown draws it, its spans adding nothing (LEGION-693).

- An agent session creates a Dispatch project (LEGION-684). `POST /api/v1/projects` accepts a
  bearer as well as a signed-in person, so a session with its token makes the project its work
  needs, with its `project.created` event naming that session, rather than asking a person to
  make it in Settings. A key that already exists is still `409 PROJECT_EXISTS` whoever asks, and a
  key that is not a project key or an empty name `400 INVALID_PROJECT`. `GET /api/v1` lists the
  route as `any`. A name is at most 80 characters (`contracts.ProjectNameMax`, UTF-16 units after
  the trim; past it `400 CAP_EXCEEDED`), and is `400 INVALID_PROJECT` when it holds a control
  character (Unicode Cc) or a bidi embedding, override or isolate (U+202A-U+202E, U+2066-U+2069), or
  has no visible character (a letter, number, punctuation or symbol other than the blank fillers
  U+115F, U+1160, U+3164, U+FFA0 and U+2800, or a code point Go's Unicode tables leave unassigned,
  such as an emoji newer than them, other than a noncharacter (U+FDD0-U+FDEF and each code point
  ending in FFFE or FFFF) or a reserved default-ignorable (U+2065, U+FFF0-U+FFF8, U+E0000-U+E0FFF);
  combining marks, format characters, private-use characters and whitespace don't count); every
  other format character (an emoji's joiner, a flag's tags, a direction mark, a soft hyphen) and
  every combining mark is allowed beside a visible one. Any bearer
  writes a name into the project list every dashboard reads and no route renames a project. Every route
  that refuses a project key that is not one (creating a project or an issue, mapping a repository,
  search) now says the rule one way, `<field> "x" is not a project key: an uppercase letter, then 1
  to 9 uppercase letters or digits, such as CORE`, from `contracts.ProjectKeyRule`, and the key
  pattern itself is generated as `contracts.ProjectKeyPattern`. Creating an issue without a title
  answers `title is required`.
- The Delivery page groups agent sessions (LEGION-682). A session group, kept in two new tables
  (migration 0093), is a stable id, a name and the sessions it holds; a pull request whose first
  session is a member has the group as its Parent agent, `group:<id>`, so the timeline's facet,
  colours, lanes, list and measures all follow the group while the Session facet keeps each
  session apart. `POST /api/v1/delivery/session-groups` and `PATCH` and `DELETE
  /api/v1/delivery/session-groups/{id}` write them on the delivery settings' rule (any signed-in
  person or agent), refusing a session another group holds (`409 SESSION_IN_GROUP`), a name another
  group has (`409 SESSION_GROUP_NAME_TAKEN`) and a name that is the title of a session outside the
  group (`409 SESSION_GROUP_NAME_IS_SESSION_TITLE`). A session that takes a group's name as its
  title afterwards shows as `<title> (<session id>)`. The timeline gains `session_groups`,
  `session_titles` and `session_pr_counts`. The dashboard edits groups from the Parent agent
  facet's **Edit groups**, chooses which Parent agents get their own colour and lane (the rest
  share **Other**), and sets the window from its header: the last 7, 14, 28 or 84 days, rolling,
  or a custom range of whole UTC days, refused past 84 days. A facet change no longer writes the
  window into the address as fixed dates.

- Dispatch's **Machine logins** list shows each login's **Credential** id, the broker's
  `credential_id` that every enrollment the login made names. A login's machine name is what its
  machine reported, so two logins of one service on one host read alike there; the id tells them
  apart, and the broker's operate page uses it to find the login behind a token-less renewal.
- `Ctrl+Alt+M` (`⌘+⌥+M` on a Mac), Google Docs' shortcut, comments on the text selected in a
  document, as the selection bar's **Comment** does: it opens the margin's composer on the
  selection and puts the cursor in it. It works in an issue's documents and a project's documents
  while the cursor is in the document and text is selected, and does nothing in a comment being
  written or on a document the reader cannot edit. The `?` help lists it under **Document** and
  shows `⌥` for Alt on a Mac. On a Mac, shortcuts that hold Alt also match the key's position,
  since Option changes the character (Option+M types `µ`); every other system keeps
  character-only matching, so AltGr continues typing its layout's character (LEGION-680).
- Every document version now carries `size`, the bytes of the markdown it stores, as file and
  image versions already carried theirs: on the issue and project artifact lists, the issue read,
  an artifact's read, each version's read, the reference closure and outgoing reference reads, the
  version an upload, a named version or an edit answers with, and the version on every
  `artifact.created` and `artifact.version` event, settlement's and a rebuild's included. Dispatch
  reads it from the version row's own stored length (`octet_length`), so listing a document near
  the 1 MiB cap neither renders it nor loads its stored history, and a person or a monitor can find
  such a document from the list alone (LEGION-481). The dashboard's issue Artifacts tab and project
  Documents tab show each document's latest size beside its kind, and a document's version list
  shows every version's.
- The conversation view (`/agents/<id>/live`) completes `/` from the session's commands in Send
  and Aside: built-ins, extension/custom/skill commands, file commands and prompt templates. It
  ranks prefix matches first, puts terminal-only rows last and never offers a list in BTW or when
  the session sent none. Picking runs the command as if typed at the session's terminal; output,
  or why a terminal-only command did not run, is the session's Reply via Dispatch (LEGION-394).
- A person lists and ends their own machines' logins and their live grants from their own shell.
  `agent-secrets machine list` and `machine revoke ID`, and `agent-secrets grant list` and
  `grant revoke ID`, call four new broker routes, `GET /v1/operator/machines`,
  `POST /v1/operator/machines/{id}/revoke`, `GET /v1/operator/grants` and
  `POST /v1/operator/grants/{id}/revoke`, which take the machine's own login (a launcher proof)
  rather than Dispatch's UI token and act for its operator, the person who approved the login. The
  grant routes answer and decide as Dispatch's Live grants page does, a revoke of a grant the
  operator's own session got without asking withholding it. The machine routes cover the person's
  own machines' logins alone: their rows of Dispatch's machine-login page, byte for byte, while a
  service's login, such as the Legion daemon's, is neither listed nor revocable there
  (`404 NOT_FOUND`) and is revoked on that page. A service's login, which has no operator, is
  refused every route, `403 SERVICE_CREDENTIAL`. The helper signs these calls with the machine's
  credential through a new `sign-launcher` op, for those four routes under its own broker alone
  (`BAD_REQUEST` otherwise). It refuses `IN_SESSION` to a process inside a registered session's
  process tree, and to one whose ancestry it cannot follow to init (a parent it cannot read, a
  loop, or past 64 processes), so an agent's own commands cannot act as its operator. The check
  covers the session's process tree only: a process the session sends out of it (`( cmd & )`,
  `setsid -f`, a tmux server it started) passes, and the same user can stop the helper anyway. Such
  a process can list and revoke the operator's own machine logins, never a service's machine
  login, and the operator's grants, which include grants the operator approved on any session. It
  can also enroll a box through the helper (`agent-secrets enroll --helper`, open to any process of
  the user) and read the operator's agent secrets. A revoke made this way records
  `launcher:<credential id>` as its actor on every row it writes; a revoke from Dispatch still
  records `human:<email>`.
  `machine list` prints the credential, host, approver, issue and expiry times and state,
  `grant list` the grant, secrets, how it was granted, approver, session, operator and expiry, and
  `--json` prints the broker's body verbatim. `machine revoke` warns on stderr when it ends this
  machine's own login, its id typed in any case.
- A request for agent secrets can ask for a shorter grant: `agent-secrets request NAME... --ttl 2m`
  and the exec form's `agent-secrets NAME... --ttl 2m -- cmd` sign the lifetime into the request
  object (`lifetime_seconds`), the record body carries it (`lifetime_seconds: 120`), and the grant
  ends that long after it is granted while its session lives on. `--ttl` takes a whole number of
  seconds from 1s to 2147483647s; `0`, a fraction of a second or anything else exits 2 before any
  broker call. `POST /v1/requests` and `GET /v1/requests/{id}` answer the `lifetime_seconds` the
  grant got (null for none), and the CLI checks it: a broker or helper from before `--ttl`, which
  drops the claim and grants for longer, has the grant or pending request this call made revoked or
  cancelled at once, and the command exits 1 naming why, running nothing. What an earlier call
  made is left alone and named: an approved grant such a broker reuses, and a pending request it
  answers this call joined (`coalesced`). A request with `--ttl` never reuses a live grant of
  another lifetime.
- A pending secret request names whom it waits on. `POST /v1/requests` and
  `GET /v1/requests/{id}` answer `approver`: the approver the request's credential-request record
  names, a person's Dispatch login or `anyone` for a shared secret, read from the record rather than
  the current policy, so it is the person whose Inbox lists the request even after the secret's
  owner tag changes; null when no person decides. `agent-secrets request` and
  `agent-secrets status` print, after a waiting request's id and state, a line naming that approver
  and where they decide it (`waiting for ada@example.com to approve it in Dispatch: <record page>`),
  and the exec form's wait prints the same line in place of `approve or deny it …`. `--json` prints
  the field verbatim; exit codes are unchanged (LEGION-666).
- The secrets broker can sign in to an Amazon RDS or Aurora database by IAM token. When
  `BROKER_DATABASE_URL` names a user and no password and its host ends in `.rds.amazonaws.com`, every
  new pooled connection, and the migration lock watch's own connection, signs in with an RDS IAM
  auth token minted for that user and host from the AWS SDK's default credentials, in the region
  `AWS_REGION`, `AWS_DEFAULT_REGION` or the shared AWS config names (it needs `rds-db:connect` on
  the database user), so no database password exists for RDS to rotate under the broker. Such a URL
  must name that one host with `sslmode=verify-full` and an `sslrootcert` file, or the broker
  refuses to start naming the host, since a token is a password for 15 minutes,
  `sslmode=require` verifies nothing and `sslrootcert=system` holds no RDS CA. A password pgx reads
  for the URL (`PGPASSWORD`, a passfile) keeps it on that password. The Envoy image ships the RDS global CA bundle at
  `/etc/ssl/rds/global-bundle.pem`, outside the system trust store, so no binary in the image
  trusts an RDS CA for any other connection. A URL with a password, or any other host, connects as
  before (LEGION-662).
- `GET /api/v1/me/answers` lists a person's own answers and replies on asks, newest first,
  with whether each answer is still current. `POST /api/v1/asks/{id}/answer` takes
  `expected_answer_at` to change the current answer; the change is another `ask.answered`
  carrying `previous_answer`, and `GET /api/v1/asks/{id}` lists the ask's `answers`. Migration
  `0082_answers_by_person` adds the partial indexes the list reads (LEGION-622).
- An agent's conversation owns the files and images sent in it, a third artifact owner beside an issue and a project: `POST /api/v1/agents/{session_id}/artifacts` takes a multipart upload with an issue upload's caps, errors and file-store write, and `.../artifacts/{slug}` and `.../artifacts/{slug}/versions/{n}` read one and its bytes; there is no list route. Such an artifact carries `session_id` (null on every other artifact), `ref_key` `agent/<session_id>/<slug>`, and is addressed `dispatch://agent/<session_id>/artifact/<slug>[@vN]`, which the reference graph indexes inside a picture's `![name](…)` as anywhere else, as it does the dashboard page `/agents/<session_id>/artifacts/<slug>[?v=N]`. It holds files and images only: a markdown document is `400 ARTIFACT_INPUT`, its file takes no comment, ask or subscriber (`400 ARTIFACT_AGENT_OWNED`), its upload appends no event, and a session id holding `/`, `?`, `#`, whitespace or a control character is `400 INVALID_SESSION_ID`. Every file or image version is now served with `Cache-Control: private, max-age=31536000, immutable`, since its bytes never change; a document version is not. Migration `0071` adds the column, the one-owner check and the partial index `artifacts_session_id`, which the upload's lookups of a conversation's artifact by name and by slug read, and turns `ref_key` into a trigger-filled column without rewriting the table; its census answers `0` (LEGION-541).
- `POST /api/v1/issues`, `POST /api/v1/issues/{key}/asks` and `POST /api/v1/artifacts/{id}/asks`
  now return `advice.suggestions`: the three fused search hits (sjawhar/legion#1764) most like
  what was just filed, and, for an ask, any already-answered ask that settles the same question,
  with who answered and when. Search runs after the write has already committed, scoped to the
  write's own project and bounded by `writeSuggestionTimeout` (300ms), so a slow or down search
  never holds up or refuses a write; `suggestions.missing` says why instead. Everything the
  write's owner holds is left out (the new issue and its spec; the issue or project document an
  ask sits on and everything inside it), and a hit on an issue that is `done` ranks below every
  hit on an open owner, so an issue already closed as a duplicate never displaces the open issue
  it was closed into. Every offered suggestion is recorded in a new `write_suggestions` table,
  whose `outcome` the new `api.RunSuggestionOutcomeSweep` background loop (every
  `SuggestionSweepInterval`, a minute) advances from `ignored` to `acted_on` (the suggested item
  was cited from the source, or the suggested issue was updated directly) or `overridden` (the
  source instead got further activity), so how often the suggestion was right can be counted
  later (LEGION-550).
- `DISPATCH_FILE_STORE_BUCKET` moves uploaded files (images, attachments; never a document's markdown) out of Postgres into an S3 bucket, under `files/sha256/<hash>`, one object however often a file is uploaded, through the AWS SDK's default credential chain (a configuration naming no region refuses the boot). An upload writes its object before its row and before it takes any lock or connection, within 30 seconds; the row keeps the file's name, type, size and hash and no bytes, and a store that refuses answers 502 `FILE_STORE_UNAVAILABLE` and writes no version. The version route serves a row still holding bytes from the row and any other from the bucket: it opens the object within 30 seconds, sends its headers and `Content-Length` only then, streams the body at the client's pace while checking it against the row's hash, and cuts a body that fails or hashes wrong mid-stream rather than ending it as if whole; a bucket that does not answer is 502, an object the bucket does not hold is 500 `FILE_MISSING`, and a cleared row on a server with the setting unset is 503. `/healthz` reports the bucket as `files` (null when unset, else whether a two-second `HeadBucket` answered, probed beside the database probe) and never fails on it, since one task's probe decides whether Dispatch is up at all. `envoy-dispatch backfill-files` moves the files uploaded before the bucket, oldest first and one row at a time, writing each object, reading it back against its hash and only then clearing the row; a row it cannot move is printed `FAILED version <id>` and passed over, the run exits 1 when any failed and can be run again at any time, the server serves each row until it is cleared, and a second run at once is refused by an advisory lock. `--verify-only` reads back every cleared row and exits 1 naming any whose object is missing or wrong; `--restore` is the rollback, writing every cleared row's bytes back from the bucket so a server without the setting, or an image from before it, serves every file from its row. Unset, every upload stays in Postgres as before. The store's tests run the real client against a loopback fake and against an S3-compatible server in a container (`internal/tests3`, SeaweedFS, since MinIO's image left Docker Hub) (LEGION-520).
- `DISPATCH_ASSET_STORE_BUCKET` lets Dispatch serve a missing, content-hashed `/assets/*` file from the same key (`assets/<file>`) in a retained-assets bucket, so a tab left open across a deploy keeps loading its build's chunks. Dispatch reads the whole object, at most 8 MiB, within three seconds before it answers, keeps the immutable cache contract on a hit, and never serves a page or non-asset path from the bucket. An absent object is still a 404; any other store failure, a short body or an object over the limit included, is a 502 with no cache header, logged at ERROR with its key. At most 64 MiB of retained objects are held at once, since a client that stops reading keeps its object in memory: a request waits within its three seconds for room, so a stale tab's burst of chunks is served in turn, and one that cannot get room in time, or whose admitted fetch then runs out of that same bound before the bucket answers, is a 503 with `Cache-Control: no-store` and `Retry-After: 1`, logged at WARN with its key — the shared bound expiring is never presented as a store failure, since the store was only asked too late to answer in time — and a room refusal asks the bucket nothing. A client that disconnects while still queued for room logs no more than a debug line and gets no response. The task role needs `s3:GetObject` on `assets/*` and `s3:ListBucket` on the bucket, without which S3 answers a missing key with 403 and Dispatch with 502. Leaving the setting unset preserves local-only static serving.
- `DISPATCH_AGENT_TOKEN` takes several values separated by whitespace, the first the current one, so the shared agent token can rotate with an overlap: the HTTP API and the document websocket accept every value, comparing a bearer with each in constant time. Startup refuses an empty entry (two whitespace characters in a row) or a repeated one, naming its position and never its value; one value behaves as before. A request that authenticates with a value after the first logs `dispatch: request authenticated with a previous shared agent token` at WARN, with the value's position, the rightmost `X-Forwarded-For` address (the connection's own without one), the User-Agent and the path, at most once per address and User-Agent every 10 minutes (LEGION-538).
- `envoy-dispatch settings` prints every Dispatch setting the server and its subcommands read, one row each with its `_FILE` form, default, whether it is required and a one-line description, from one table (`cmd/dispatch/settings.go`) that is now the only place Dispatch's own code reads its environment; the docs site's Dispatch configuration reference is generated from it. The libraries Dispatch links still read their own variables (`HOME`, libpq's `PG*`, Go's proxy, certificate and runtime variables), which the table does not list. Every setting resolves as before, the `envoy.json` overrides and `_FILE` forms included; the readers that used to call `os.Getenv` themselves (the dashboard directory, the GitHub App credentials, the signing key and insecure-cookie flag, the `envoy.json` overrides, the Envoy listener token, and NATS's reach and nkey) are handed their value from the table.
- `envoy-dispatch routes` prints the route table `GET /api/v1` serves, read without a database or a listener; the docs site's Dispatch HTTP API reference is generated from it.
- `envoy-dispatch census`: the pre-deploy census of the migrations a database has not recorded (the runner's own rule), for a deployment to run before it rolls the service. It prints, for each pending migration, the tables it locks above ACCESS SHARE with size, row count and the sessions holding locks on them (pid, role, application, state and transaction age, "not visible" where Postgres hides one, or "autovacuum worker"; never query text), the transactions open longer than a minute or of an age it cannot see, and the count the migration's own `<version>_<name>.census.sql` answers. Those tables are the ones its statements name (an aliased `update` and a foreign key's referenced table among them; none in a comment, a string literal or the body of a function the migration defines), the table behind an index it drops or alters, and every table a foreign key reaches from rows it writes (a cascade, or a check that a row is still referenced), read from `pg_constraint` and from the keys earlier pending migrations add, inside a `DO` block too, and checked for holders and readability but not counted. It refuses (exit 1) a census that counts rows; a table a migration names above 1 GiB, which it does not count; a lock on a touched table held by a transaction older than a minute, or by one whose age Postgres hides from the census's role (granting that role `pg_read_all_stats` lets it read the age) or does not record (`track_activities` off), but not by an autovacuum, which Postgres cancels for the migration, unless it is anti-wraparound, or may be (its activity hidden or untracked, its table past its freeze age), or `deadlock_timeout` is not shorter than the lock timeout; a touched table it cannot read within the five-second lock timeout or the statement timeout; a census that fails, a name the database lacks included, even behind an earlier pending migration that may create it; and a database that records no version yet holds tables. A fresh database, one recording no version and holding no table, passes with `census: fresh database, nothing to check`. A failure names the file and the SQLSTATE, and Postgres's message only when it points into the census's own text, so no row value reaches a log. The census exits 2 when it cannot be taken (a connection whose `search_path` names no schema that exists among the causes), and writes nothing: one read-only transaction, the census statement sent through the extended protocol whatever the connection string asks. `pgmigrate.Load` refuses a census that is not one select, that holds a Unicode escape (`U&'…'`, `U&"…"`), or that names `pg_terminate_backend`, `pg_cancel_backend`, `pg_sleep`, an advisory-lock function (`pg_try_advisory_*` included) or a function that runs a query given as text (`query_to_xml` and its kin, `ts_stat`, `ts_rewrite`), bare or quoted and in any case, anywhere outside its comments and string literals, which it finds as Postgres 16's lexer does; the census runs with `standard_conforming_strings` on, so Postgres reads its literals the same way. Every migration from `censusRequiredFrom` (56, `internal/dispatch/store/store_test.go`) declares a census; 0053, 0054 and 0055 carry worked examples. Each store's tests hold the census's reading of every migration to the locks it really takes, run every shipped census at the schema just before its migration, and require a census that reads what a migration from `censusRequiredFrom` on creates, renames or gives a new type to name that migration, since a release carrying both refuses every deploy. An unknown `envoy-dispatch` subcommand now exits 2 instead of serving, which migrated the database (LEGION-459).
- `DISPATCH_DEV_SIGNIN=1` mounts `GET /auth/_dev/signin?login=<email>&next=<path>` on Dispatch, which signs the person it names in, lowercased, with the same session cookie a sign-in-pool sign-in issues, with no pool step, so a local instance can be driven signed-in. Boot refuses the flag unless identity is cookie, no `DISPATCH_SIGNIN_*` setting is set, the listen address is a loopback IP literal, the dashboard origin names `127.0.0.1`, `[::1]` or `localhost`, every `DATABASE_URL` host is loopback or a unix socket, `DISPATCH_SIGNING_KEY` is unset, `ENVOY_ALLOW_REMOTE_NATS=1` is not set while NATS is on, a set `DISPATCH_AGENT_SECRETS_URL` names a loopback host, `ENVOY_URL` names a loopback host, and a loaded GitHub App private key comes from `DISPATCH_APP_PEM_B64` with `DISPATCH_GITHUB_API_BASE` naming a loopback host, since a signed-in session can have the App probe and import any repository it is installed on. A key in `app.json`, where a developer keeps the real App's key, is refused whatever the base, naming the file; the environment's key must be a throwaway, as `packages/dispatch/e2e/run-server.sh` generates one, because every App call hands a signed App JWT to whatever listens at that base. These refusals come before NATS or Postgres is dialled. The signing key is then generated per process, so a minted cookie is worthless on any other server and dies with the process. What a signed-in session writes to the database (a `dsp_` token it mints, the session rows its sign-out changes) is honoured by every server on that database, so a dev-sign-in server needs a database of its own. While the flag is on, every request must carry the dashboard origin as its `Host` (`421 HOST_MISMATCH`), and the route serves only a loopback peer with no forwarding header (`403 DEV_SIGNIN_FORBIDDEN`) and logs every mint at WARN. `DISPATCH_LISTEN_HOST` may now be an IPv6 literal with or without brackets: `::1` listened on `::1:8766` and failed with "too many colons", and now listens on `[::1]:8766`. A bad `DISPATCH_PORT` now refuses the boot before NATS and Postgres connect.
- Every cache warm-up logs one line when its initial scan ends, `<cache> cache warm-up`, with the bucket, `elapsed_ms`, `entries`, `delete_markers` and `outcome` (`completed`, or `timed out` at WARN when nats.go's idle timer gave up on the scan). The counts are disjoint, like the role restore line's: `entries` is the live keys the scan delivered, `delete_markers` what it streamed past to find them. The line goes through the logger the listener passes each cache (`store.WithLogger`, `session.WithSessionLogger`, the CI store's own), so it is a JSON record with `machine_id`, and so are the cache watchers' other lines (a failed first start, a recreated bucket, a watcher that stopped). A restart now names its own cost — measured on a listener with a seeded bucket: `{"msg":"interest registry cache warm-up","machine_id":"smoke374r2","bucket":"envoy_interests","elapsed_ms":5,"entries":3,"delete_markers":500,"outcome":"completed"}`. A timed-out scan is logged and never recorded as the watcher's terminal error, so a live watcher whose warm-up merely timed out is not a 503 or a rebuild.
- A listener start logs one INFO line for the role-claim snapshot it restores, `restored role claims`, with two disjoint fields: `restored`, the claims whose revision the scan read and which therefore keep their holder's restart grace, and `delete_markers`, the tombstones the scan streamed past to find them (production on 2026-09-28: `restored=9 delete_markers=757`). They are separate on purpose — one total of both reads as claims the restart failed to restore. `internal/store`'s lines, this one and both reaper cycles, now go through the logger the listener passes it (`store.WithLogger`), so they are JSON records carrying `machine_id` like the rest of the listener's output. `internal/bus`'s lines and the stdlib `log` package's stay in Go's text format, which the deployed CloudWatch metric filters for publish failures, webhook refusals and dropped stream subjects match on.
- A CI record write that runs out of its two-second retry budget logs one JSON line, `ci record exceeded its retry budget`, naming the head (`owner`, `repo`, `number`, `sha`), the record's `checks`, the write's `attempts`, the `observations` it answered 503 and the last attempt's `error`, so an alarm can count the listener's 503s by that cause.
- Added the native Dispatch workspace API, persisted documents and events, retained Dispatch notifications, and daily Postgres backups.
- `POST /v1/roles/set` accepts `"soft": true`: the claim lands only if the role is unheld, held by a session that is no longer live, or held by the declared `previous_session_id`; any other live holder answers 409 with its id.
- Dispatch redelivers the GitHub App webhook's failed deliveries, which GitHub never redelivers on its own. Every two minutes it lists the webhook's attempts whose status is not OK and asks GitHub to redeliver each one the listener answered 5xx or GitHub could not complete. A failed or refused redelivery is retried after a doubling backoff, at most five times, and a 4xx is never redelivered. Requests go a second apart, and a GitHub rate limit stops the sweep until the time GitHub gives. `envoy-dispatch redeliver-webhooks --since <d> [--dry-run]` runs the same sweep over a chosen window.
- Every Dispatch message read returns `broadcast_id`, the broadcast the message is one
  recipient's copy of, or null: the thread read (`GET /api/v1/messages/{id}`), the Agents page's
  conversation list, each reply, and every message event (LEGION-394).
- `POST /api/v1/messages/{id}/deliveries/{attempt}/accept`: the attempt's session records, once
  per message, that it took a person's fresh, latest attempt of a direct message to it, requested
  by the message's own author, as its user's own turn, and is answered that attempt with the
  message's stored `body`. Anything else is refused with a 409 naming the check
  (`ACCEPT_NOT_DIRECT` for a message on an issue, a broadcast's copy or a reply in a broadcast's
  thread, `ACCEPT_NOT_WRITTEN_BY_PERSON`, `ACCEPT_NOT_ASIDE_OR_STEER` for a BTW,
  `ACCEPT_ALREADY_ACCEPTED`, `ACCEPT_SUPERSEDED`, `ACCEPT_NOT_REQUESTED_BY_PERSON`,
  `ACCEPT_NOT_REQUESTED_BY_AUTHOR` for another person's retry, `ACCEPT_FAILED` for an attempt
  Dispatch recorded as failed, `ACCEPT_STALE`) or 403
  `ACCEPT_FORBIDDEN`. It appends `message.accepted`, which reaches the dashboard's event stream
  and never NATS, since the outbox publishes no issue-less event. Every delivery attempt now reads
  `requested_by` (who asked for it, kept on a resume, null before migration 0054), `accepted_at`
  and `accepted_as` (LEGION-394).
- `POST /api/v1/broadcasts` requires `idempotency_key`, naming one send (letters, digits, `.`,
  `_`, `:` and `-`, at most 128 characters), so a repeated create no longer hands every recipient
  the message twice. A repeat by the same human with the same key, body, mode and `session_ids`
  is answered `200` with the broadcast the key made, even while the listener is down; the same
  key with a different request is `409 BROADCAST_KEY_REUSED`, naming that broadcast as
  `broadcast_id`, and sends nothing; two requests carrying one key at once write one broadcast.
  A missing key or one with another character is `400 BROADCAST_INPUT`, and one over 128
  characters `400 CAP_EXCEEDED`; only the missing-key text adds what a page loaded before this
  change should do (restore its draft, copy the message, reload and send again). Keys are stored
  per human in `broadcast_idempotency_keys` (migration `0055`) and kept as long as their
  broadcast (LEGION-446).
- Each request a listener's starting gate refuses logs `request refused while starting` with the
  `gate` (`webhook` or `v1`), `method` and `path`; a start logs `envoy-listener /v1 open` with
  `since_listening_ms` and `durable bound` with `attempts` and `waited_ms`.
  `packages/envoy/scripts/listener-deploy-probe.sh` watches a listener deploy from a client's
  seat, at every address the listener's name resolves to, and exits 1 when a task refused `/v1`
  while it answered `/healthz`, or, with `--dispatch-*`, when a Dispatch message it posted did not
  record state `sent` (LEGION-456).
- `GET /api/v1/artifacts/{id}/blocks/{block_id}` says where one block stands in a Dispatch
  document: its path from the top-level block down (each node's type, block id and child index)
  and, for a table block, row or cell, the table's id, the row index (0 is the header row), the
  cell's column index (the indexes `delete_row` and `delete_column` take), the text of the header
  cell drawn above it (in a table with colspans or rowspans, the column the cell is drawn in) and
  the row's cells. An id the live document does not hold is `404 TARGET_NOT_FOUND`.
  `GET /api/v1/comments/{id}` and `GET /api/v1/asks/{id}` carry the same answer for their
  anchor's block as `anchor_block`, derived from the live document at read time and absent when
  the anchor names no block or the block has left the document; lists and events do not carry
  it. When the anchor's document cannot be read, those two reads still answer `200`, without
  `anchor_block` and with `anchor_block_error` (`DOC_SERVICE_UNAVAILABLE`, `DOC_SCHEMA` or
  `INTERNAL`, the codes the API answers those errors with elsewhere), logged at WARN, and they do
  not wait for a failed document room's recovery; only a request that has itself gone away fails
  them (LEGION-460).
- `PUT /api/v1/me/agents/{session_id}/state` accepts `read_replies`, the ids of messages that
  session wrote, and marks those replies read and no other (`user_agent_reply_read`, migration
  `0067`); `unread_replies` and the conversation window's unread flag leave them out. Opening a
  broadcast page now clears the answers it shows from the New replies badge and each agent's row,
  on every tab, while the session's older reply to another message, or one newer than the page
  shows, still counts. An id may take any form `uuid.Parse` reads; one it cannot read, or one that
  is not a message that session wrote, is `400 INVALID_STATE`. A request naming only replies
  already read by id, or replies the session's read mark has passed, changes nothing and appends
  no `user_agent_state.updated` event. A reply the viewer's Clear hides but the read mark has not
  passed is neither, though the count leaves it out: a Clear can move back, so the first request
  naming that reply writes its row and appends the event. A `read_through` deletes the viewer's
  rows for that session whose replies it reaches (LEGION-485).
- A Dispatch document edit's response reports `advice.decision_blocks_added`: how many ask blocks
  the edited document holds whose block id no ask block in the document before it carried, read
  from the parsed trees the edit was applied to. A block inserted in a blockquote, on a list item's
  line or behind four or more colons counts, as does a block retyped into an ask; an opener inside
  a fence or indented code counts nothing, nor does a block the edit moved or reworded. An issue
  document's edit carries it beside the issue advice; a project document's edit, which before
  carried no advice, now carries `advice` holding the count alone (LEGION-470).
- The secrets broker logs at boot how it signs in to its database, `database sign-in
  method=rds-iam` or `method=password`, before it reaches for AWS or the database, so an operator
  tells a token sign-in from a password one in the log rather than the task's environment.
- `GET /api/v1/search` gives every `ask` hit `ask`, where the ask stands when the search ran:
  `open`, `answered` with its latest answer (who gave it, when, the options it chose and its
  text), or `resolved` with its resolution and, when anyone answered it, the last answer it had,
  which for an ask that document settlement retracted while it was answered is read from its
  `ask.answered` events, since its row keeps none. One query reads every ask hit of a page, the
  events are read only when the page holds a resolved ask, and a page with no ask hit reads
  nothing more. The search palette shows it under each ask hit's excerpt: a badge (`Open`,
  `Answered`, `Closed, no answer` or `Closed after an answer`), who closed the ask and when, and
  its latest answer, with who gave it, when, the options chosen and its text cut to three lines
  (INFRA-569).

### Changed

- **Breaking:** `envoy-infra:gcp:Dispatch` takes two new required inputs: `tailnetService`, the
  Tailscale Service the front door hosts, which must resolve to `svc:<hostname>` (pass the `Name`
  output of the consumer's `tailscale.Service`, so the Serve ConfigMap and the front door wait for
  the Service), and `frontDoorToolsImage`, the digest-pinned image `cert-publish` runs on, which
  must carry `/bin/sh`, `curl`, `openssl` 3, `base64`, GNU `date`, `sed`, `grep` and `tr` (the
  envoy image does). The front door's shape changed: its pods run as a ServiceAccount of their own,
  `<hostname>-tailnet`, whose Role `<hostname>-tailnet-tls` may only get and patch the certificate
  Secret, with the token mounted into `cert-publish` alone; the Role `<hostname>-tailscale-state`
  is gone; and a Tailscale Service holds the name `<hostname>` rather than a node. Moving takes
  steps before the update ("Moving from a single front-door node" in
  `packages/envoy/infra/README.md`): give every client of Dispatch access to `svc:<hostname>` on
  443 in the tailnet policy (a grant with that destination, or a tag on the Service their grants
  already name), since grants on the front door's tags do not cover the Service's address, with
  an auto-approval of the Service for the front door's tags; delete the tailnet machine that holds
  `<hostname>`; declare `svc:<hostname>` with its port `tcp:443`; optionally seed
  `<hostname>-tailnet-tls`; delete a hand-made budget named `<hostname>`; and back up, then
  delete, the runtime Secret `<hostname>-tailscale-state`. The move is an outage while the old
  node is gone and the new pods come up. New refusals, each naming its input before anything
  registers: a resolved `tailnetService` other than `svc:<hostname>` (never echoing it), an
  unpinned `frontDoorToolsImage`, a `databaseAvailabilityType` other than `ZONAL` or `REGIONAL`,
  an `appCpu`, `appMemory`, `proxyCpu` or `proxyMemory` not above 0 or outside Autopilot's bounds
  for the pod's sum, a `frontDoorReplicas` or `listenerReplicas` below 1, and a
  `frontDoorPdbMinAvailable` or `listenerPdbMinAvailable` outside 1 to its Deployment's replica
  count. The defaults changed size too: `databaseTier` is `db-custom-4-15360`
  (was `db-custom-1-3840`) and the `cloud-sql-proxy` sidecar 1 CPU and `2Gi` (was `200m` and
  `256Mi`), so a deployment that relied on either default changes size on its next update; set
  `databaseTier` to the tier the instance runs to keep it. `appPriorityClassName` is renamed
  `priorityClassName` and names the class of every pod of the three Deployments (the app, the
  front door and the listener), read once, with all three Deployments waiting on the read and the
  same refusals; a program that sets `appPriorityClassName` sets `priorityClassName` instead.
- `envoy-infra:gcp:Dispatch` defaults `databaseTier` to `db-custom-4-15360` (4 vCPU, 15 GB), from
  `db-custom-1-3840` (100 connections). During a roll, every app pod that holds connections (the
  replicas, the surge pod and pods still terminating, for up to their grace period) times the
  server's `store.ConnectionsPerProcess` must fit in the instance's `max_connections` less
  PostgreSQL's 3 superuser-reserved slots, so a consumer sets `databaseTier` for its replica
  count. It defaults the `cloud-sql-proxy` sidecar to 1 CPU and `2Gi`, the proxy's own sidecar
  example's size for one pod's pools, through whose TLS every database call passes; with the
  app's defaults the pod requests 2 CPU and `6Gi`.
- `envoy-infra:gcp:Dispatch` takes each Deployment's update strategy from its own replica count:
  `Recreate` at one pod, since a roll would run two builds side by side, and `RollingUpdate` with
  `maxUnavailable: 0` and `maxSurge: 1` above one, so a new pod is ready before an old one goes
  down. The front door and the listener were always recreated; the front door, at its default of
  two pods, now rolls.
- `envoy-infra:gcp:Dispatch` spreads each Deployment's pods: required pod anti-affinity on
  `kubernetes.io/hostname`, so a node holds at most one, and a `topology.kubernetes.io/zone` spread
  with `maxSkew: 1`, `DoNotSchedule` and `minDomains: 2`, without which Autopilot, which creates
  nodes on demand, could place two pods in one zone.
- `envoy-infra:gcp:Dispatch` gives every serving container a startup and a liveness probe that
  fails only when the process does not answer: `envoy-dispatch` and `envoy-listener` run curl
  without `-f` against their `/livez`, which reads nothing (`envoy-dispatch` answers it 204, the
  listener 404 from its router), so neither a dependency outage nor a slow Postgres or NATS
  restarts a pod, and `envoy-dispatch`'s startup allows 30 minutes for its migrations;
  `cloud-sql-proxy` runs with `--health-check` and is probed on `/startup` and `/liveness` on port
  9801; and the front door's `tailscale` container's liveness asks its LocalAPI. The listener's
  readiness probe allows 6 s, and the listener waits 5 s before it stops, inside a 35 s grace
  period.
- `envoy-infra:gcp:Dispatch` rolls the pods that read one of its own Secrets when that Secret
  changes: each pod template carries `checksum/<secret>`, the resourceVersion of
  `<hostname>-dispatch` and `<hostname>-listener` on the app, `<hostname>-listener` on the
  listener and `<hostname>-tailnet-auth-key` on the front door, so a rotated value no longer needs
  a `kubectl rollout restart`. The NATS seed, the GitHub App key and the Azure key, which the
  component does not own, still do.
- Dispatch closes a client connection after its next response once the connection is 60 s old
  (`Connection: close`), except a request carrying `Upgrade`, a websocket handshake. kube-proxy
  and the front door spread load per connection, so while callers reuse keep-alive connections a
  process that has just rolled in gets little traffic; the client's new connection is spread across
  every ready process.
- Dispatch accepts an ask question of up to 2,000 UTF-16 units, from 800, on `POST .../asks` and
  `PATCH /api/v1/asks/{id}`, read from the generated `contracts.AskQuestionMax`; an approval
  request's `summary` gets what its question leaves of the same limit.

- `POST /v1/interests/subscribe` no longer stores a topic reaching a private stream subject other
  than the session's own agent subject (`bus.ReachesPrivateStreamSubject`): another session's
  `notifications.agent.<id>`, a failed-delivery lane
  `notifications.envoy.exceptions.notifications.agent.<id>` (the session's own included), a
  Legion daemon notice under `notifications.legion.>` (the controller lane
  `notifications.legion.<project>.controller`), or a wildcard overlapping one
  (`notifications.agent.*`, `notifications.agent.>`, `notifications.envoy.>`,
  `notifications.>`, `notifications.*.<x>`), since the stored topics drive the listener's fan-out
  to serve-port sessions and such a topic would hand one session another's inbox, a failed
  delivery's payload or the controller's notices (`/v1/replay` refuses such a subject on its own).
  The rest of the registration is stored and the answer stays 200, its `warnings` naming each
  topic left out (`<topic> was not registered: …`), so a released pi-envoy, which registers a fork
  parent's agent subject with the rest of what it follows, keeps registering. A row an earlier
  build stored with such a topic is cleaned at the session's next registration: the registry drops
  every such topic from the merged row, not only from the incoming topics, as do a role claim and
  an unsubscribe that rewrite the row, so the fan-out stops reaching the session after its next
  heartbeat. pi-envoy and claude-envoy no longer register or replay a topic reaching a private
  stream subject other than their own agent subject (`reachesPrivateStreamSubject` in
  `@legion/envoy-client/replay`, mirroring `bus.ReachesPrivateStreamSubject`) (INFRA-439).
- A dead session's subscriptions now stay 5 minutes longer: the interest reaper deletes one only
  15 minutes after its row's last write, where it was 10, because it now adds
  `store.InterestRefreshAge` to its grace window. For that time the subscribers page and the
  `envoy_active_interests` gauge still list them, and a message matching one logs a failed
  delivery to that session; a paused session that comes back within it still has them.
- A rate-limited answer to any minter call, a release with no narrowing and a Google login's
  refresh included, is now logged `broker: mint failed … class=upstream` with `…, a rate limit` in
  its error, where a 4xx one was `class=refused`: a 429, GitHub's `X-RateLimit-Remaining: 0`, or a
  body naming a rate limit (GitHub's secondary limit, Google's `rateLimitExceeded`). A rate limit
  is not the App owner's to fix. The release's answer is unchanged, `502 MINT_FAILED`.
- An issue read with a route no longer calls the Envoy listener and decodes the whole live-session
  list each time. Dispatch keeps one copy of that list per process, at most 5 s old, and one
  listener call refreshes it for every issue read that asks while the call runs. A listener that
  fails or does not answer is asked again only after 5 s, so the reads in that window read
  `route_status: unknown` at once instead of each waiting up to 2 s. A `route_status` can
  therefore be up to 5 s old; its values and the `503 ENVOY_UNAVAILABLE` of a `live` or
  `no_holder` filter are unchanged.
- **Breaking:** `envoy-dispatch backfill-embeddings` is removed: the server's coverage pass does
  its work at boot and every 10 minutes, and an operator script that still runs it exits 2.
  Migration `0119` drops its checkpoint table, `embeddings_backfill_progress`, in the same release,
  since no server of any release reads it. Each vector now records its embedder's identity
  (`us.cohere.embed-v4:0` on Bedrock, the value every stored vector already carries, and
  `azure:<endpoint host>/<deployment>` on Azure), and search compares a query only with vectors of
  its own identity: run one embedder configuration on every process of a deployment, since each
  process's pass drops the others' vectors, and put a new model on Azure under a new deployment
  name. A text with nothing to embed (empty or only white space) is settled with no vector rather
  than retried until it dies, and a row that died that way is settled by the pass (INFRA-393).
- **Breaking (local development):** `agent-secrets-devrelay` presents a listed caller's own
  ServiceAccount token, read from `--account-token-file` (default
  `$AGENT_SECRETS_ACCOUNT_TOKEN_FILE`), in place of the shared bearer the broker no longer takes. A
  missing or empty file exits 1 naming it, before any request. `scripts/dev-broker.sh` starts
  `dev-oidc-issuer`, a new local stand-in for a cluster's ServiceAccount token issuer (the Go
  tests' `internal/oidc/oidctest` served on a loopback port), and its broker trusts it, lists
  `system:serviceaccount:example-ns:example-dispatch` as its caller `dispatch` and turns the token
  exchange on for `system:serviceaccount:example-ns:example-worker`; the issuer keeps both tokens in
  the workdir and rewrites each before it lapses, and the script exports
  `AGENT_SECRETS_ACCOUNT_TOKEN_FILE`, so devrelay approves records and machine logins locally
  again. The Dispatch e2e harness's broker token is `DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE`.
- `DISPATCH_APP_PEM_B64` takes the GitHub App's private key as a plain PEM, as GKE SecretSync
  copies it from Secret Manager, or as the base64 of a PEM, as before: a value holding `-----BEGIN`
  is the PEM itself and any other is base64-decoded. Dispatch parses the key when it loads the
  variable, so a key that does not parse stops boot with `DISPATCH_APP_PEM_B64: plain PEM:
  <reason>` or `DISPATCH_APP_PEM_B64: base64 of a PEM: <reason>` (a value that is not base64 at
  all, `DISPATCH_APP_PEM_B64: <reason>`), never quoting the key, where it stopped boot with
  `parse GitHub App private key: <reason>`, naming neither the variable nor the form (INFRA-390).
- A failed NATS reconnect attempt is logged, where it was silent: one WARN a minute per
  connection, `envoy nats reconnect attempt failed`, naming the client, the servers it was
  configured with and the error, with the first failure of an outage logged at once. nats.go
  retries about once a second forever, so a broker that is down left no line at all in the
  process's log, and thousands of identical lines would name the outage no better (INFRA-182).
- A cache watcher that ends on its own logs `<cache> watcher stopped` at WARN, where it was an
  ERROR. The ERROR is now `<cache> watcher was not repaired`, logged once with the bucket, the
  recorded error and a `reason` of `repair failed` or `no repair within 1m0s`, and only when a
  repair has failed or none has run within a minute, so a stop the self-health rebuild repairs a
  moment later no longer raises one (INFRA-182).
- **Breaking:** once any secret with a `kind` tag holds a value, roll the secrets broker forward,
  never back past this release. A broker before kinds reads no `kind` tag and would serve the
  secret's stored GitHub App key or refresh token as its value. `agent-secrets secret` writes
  `mint-service-account: kind-secret` beside every kind, which every broker from
  legion-envoy-v13.19.0 up to this release refuses as `mint-target-malformed`, and `retag --kind
  none` removes it with the kind. A broker older than legion-envoy-v13.19.0 reads no such tag and
  serves the value, and so does every earlier broker for a kind secret written without the tag. To
  go back anyway, first disable every kind secret's versions.
- **Breaking:** the secrets broker serves a person's machine login only through the OAuth device
  grant. `POST /v1/launcher-credentials` and its poll `GET /v1/launcher-credentials/{pending}`,
  the login helpers before legion-envoy 14 used, are gone: the POST answers `405 Method Not
  Allowed` (Dispatch's `GET` on that path remains) and the poll `404`. Migration 0017 drops their
  `machine_login_polls` table. A broker of the release before this one still boots on the
  migrated database, its two removed routes answering 500. Dispatch's routes are unchanged:
  `GET /v1/launcher-credentials`, `POST /v1/launcher-credentials/{id}/revoke-by-approver`,
  `POST /v1/machine-logins/lookup` and the record decisions, as is the Legion daemon's
  `POST /v1/launcher-credentials/token`. A login such a route opened and left pending is still
  decided by its code; its machine learns nothing of it and logs in again.
- **Breaking:** `agent-secrets-helper` answers `enroll-box` and `unenroll-box`, the ops of an
  `agent-secrets` before legion-envoy 10, `BAD_REQUEST unknown op`; `agent-secrets enroll` and
  `unenroll` ask only `enroll-session-key` and `unenroll-session-key`, never falling back to a
  helper before legion-envoy 10; `agent-secrets enroll` takes no `--kind`, so `--kind box` exits 2
  naming the flag; and `POST /v1/enrollments` refuses the kinds `host` and `box`, which only such a
  helper sent, `400 INVALID_KIND`, where it recorded them as `session`.
- **Breaking:** the secrets broker needs `BROKER_MACHINE_LOGIN_URL`, the absolute http(s) URL of
  Dispatch's machine-login page (`https://<dispatch>/credentials/machine`), and refuses to start
  without it. The device authorization answer's `verification_uri` names that page, where the
  person types the machine's code.
- **Breaking:** `agent-secrets-helper` logs a machine in only through the broker's OAuth routes
  (`POST /v1/oauth/device_authorization` and `POST /v1/oauth/token`), so it needs a secrets
  broker of this release: against an earlier broker, `agent-secrets machine login` fails. Upgrade
  the broker before the helpers. The broker still serves `POST /v1/launcher-credentials` and
  `GET /v1/launcher-credentials/{pending}` for helpers from earlier releases, whose logins it never
  refreshes. (Superseded by the Breaking entry under Changed on serving a person's machine login
  only through the OAuth device grant: those routes are removed.)
- `BROKER_LAUNCHER_CREDENTIAL_SECONDS` defaults to 2592000 (30 days), up from 604800 (7 days), and
  is how long a machine login lasts without a refresh: an approval mints its credential for that
  long, and each refresh moves its expiry to that long from then. Its maximum stays 2592000.
- The Envoy listener answers a message route the bus did not serve with
  `503 {"error":"envoy: the message bus did not answer: <reason>"}` and `Retry-After: 1`, in place
  of the 500 that often reached no caller: `POST /v1/messages/send` and `POST /v1/messages/publish`
  each take a 3.5 s budget from the request's own context, which the send's session read, the
  publish route's resolution of a role lane's holder (its claim read, its holder's session read
  and the release of a lapsed claim) and both routes' publish run inside, and the publish route
  answers that same 503 for a role-holder read that did not answer or that the budget ran out on,
  so a role store that does not answer is reported inside the caller's attempt. A publish the
  NATS server denies under this listener's own grant is not an outage and is not retryable: it is
  `403 {"error":"envoy: the message bus denied the publish under this listener's own permissions: <reason>"}`
  with no `Retry-After`, since the grant forbids the subject until an operator changes it. Only
  the publish route can be denied this way, and only for a topic the stream does not capture (a
  role lane or an ad-hoc topic, published over core NATS): a send publishes
  `notifications.agent.<id>`, which the stream captures, so it goes through JetStream. A
  publish that lost the race for a role's claim — every resolution attempt spent on a supersession
  — keeps the 503 and the `Retry-After: 1` but answers
  `{"error":"envoy: the holder of role <role> changed while this publish was resolving it"}`.
  `GET /v1/roles/<role>` resolves its holder within the same 3.5 s budget, and answers that race,
  which it answered 500, with the same 503 and `Retry-After: 1`, worded
  `{"error":"envoy: the holder of role <role> changed while this lookup was resolving it"}`; a
  role store that did not answer within its cap or the budget, which it also answered 500, is the
  bus-unavailable 503 with `Retry-After: 1`, as on the publish route. A role claim record no read
  can decode (one naming no holder, or JSON that is not a claim) is, on both routes,
  `500 {"error":"envoy: the claim record of role <role> is unreadable: <reason>"}` with no
  `Retry-After` and not the bus-unavailable 503: the bus answered it, and a retry reads the same
  record. Both message routes read the
  sender's interest record for the envelope's `sender` within a second of that budget
  (`senderStampReadTimeout`): a read that does not answer stamps the session id and the title this
  listener's cache holds, and the publish goes on to meet the silent bus itself and answer the
  503 inside the attempt. `GET /v1/sessions`, `GET /v1/registry/<session_id>` and `GET
  /v1/interests/<session_id>` read an interest record their cache misses from the bucket within
  the same 3.5 s budget (a session row's read capped at a second, and a list's reads sharing one
  budget), where a silent bus held them for the 10 s JetStream wait: a read that does not answer
  is that 503 with `Retry-After: 1`, where the first two answered a row with no topics and the
  third a 503 with neither the named body nor `Retry-After`. An interest record the bus holds but
  this build cannot decode is not that 503: the two single-session routes answer
  `500 {"error":"envoy: the interest record of session <id> is unreadable: <reason>"}` with no
  `Retry-After`, and `GET /v1/sessions` lists that session with no topics and roles, as it did,
  rather than refusing the list, while the registry logs `registry read an interest record it
  cannot decode` at WARN naming the session. A read counts as answered only for its named answers:
  no key, that undecodable record, or an id the interest bucket refuses as a key (`bus.ErrRefused`,
  an earlier listener's id past `envoy_interests`' get bound that `envoy_sessions` still lists, or
  one outside nats.go's key alphabet), which the list shows with no topics and roles, logged
  `session listed with no topics: its interest record could not be read` at WARN naming the
  session. `GET /v1/registry/<session_id>` answers such a live session the same way, 200 with no
  topics and roles, logged `session answered with no topics: its interest record could not be
  read`, where it answered 413 or 400, which Dispatch's envoy client read as a session that cannot
  exist; `GET /v1/interests/<session_id>`, which asks for the record by that key, and a key the
  session bucket refuses keep their 413 or 400. Every other error is a
  read that did not answer, whatever it names, a raw socket error from a broken connection among
  them, and all three routes answer it with the 503 and `Retry-After: 1`. The routes that write -
  `POST /v1/interests/subscribe`, `POST /v1/interests/unsubscribe`, `POST /v1/roles/set`,
  `DELETE /v1/interests/<session_id>` and `DELETE /v1/sessions/<session_id>` - do every store read
  and write within the same 3.5 s budget, where a silent bus held each for the 10 s JetStream wait
  (40 s for a claim whose own write met it), and answer store work that did not answer with that
  503 and `Retry-After: 1`, where they answered a 500 or a 503 with neither the named body nor
  `Retry-After`; their 409, 413 and 400 are unchanged. A write the budget abandons may still land
  after the 503: a put writes what the caller's retry writes again, and a delete or a claim is
  conditional on the revision it read. An
  interest record this build cannot decode no longer locks out the routes that write it, which
  failed on the read and every retry: `DELETE /v1/interests/<session_id>` deletes
  it and releases the session's role claims (204, where it answered 500); `POST
  /v1/interests/subscribe` and `POST /v1/roles/set` replace the claimant's with the one the request
  names rather than merging (where they answered 503 and 500), and the registry logs `registry
  replaced an interest record it cannot decode` at WARN naming the session; the session's role
  claims, in the role bucket, are kept. `POST /v1/interests/unsubscribe` releases the role claims
  its topics name and leaves the record, which routes none of its topics, answering 200 where it
  answered 500, and a role claim taken from a holder whose record is unreadable no longer answers
  500 after it lands. The 413 for a message NATS cannot
  take whole and the 400 for a subject or key it refuses are unchanged. A publish on either route
  whose request context has already
  ended — its budget spent, which a role-holder resolution that answers just as the budget ends
  can leave it, or the request cancelled — puts nothing on the wire and is answered that 503: a
  core subject has no duplicate window and the client retries every 5xx, so a message emitted
  behind a failure answer would reach its subscriber twice. A core publish checks its context
  immediately before it publishes, one check that sees both a spent deadline and a cancel, so one
  cancelled while NATS reconnects sends nothing; a cancel after that check cannot be undone, and
  the publish goes ahead. A JetStream publish relies on nats.go,
  whose request sends nothing under an ended context. A send resolves its target from the session
  store itself rather than from this listener's cache of it — a cache miss is read through to the
  `envoy_sessions` bucket within a second, the same direct get the role paths already make — so
  `404 {"error":"no live session <id>"}` is now only the store's own word that the key is gone,
  and an agent delivery reads that same store before it pushes. A delivery this listener cannot
  place — no session record, a record naming nothing to push to, or a push that failed — is NAK'd
  and redelivered every five seconds, counted from the message's first delivery rather than from
  its age in the stream and stopping at 18 deliveries. The first two causes push nothing, so a
  retry repeats nothing, and they are deferred as many times as the new
  `ENVOY_UNPLACEABLE_RETRY_WINDOW` (default `60s`) buys at that delay, twelve at the default. A
  push that failed is deferred once however long the window, and its give-up is final whether or
  not the give-up's notice can be published, so it is pushed at most twice: it may have landed,
  since a refused prompt, a connection that never reached the session and an answer lost after
  the session server took the prompt all come back as the same failure, and a retry that clears
  the attempt record to push again would repeat an instruction the session may already have, once
  per retry. At most 64 such messages are held deferred at once by the whole
  listener (a quarter of the durable's ack-pending budget, one slot of which each deferral holds
  for its whole window), in one budget shared by every delivery handler it builds — the one a
  rebind after a NATS flap builds included — and keyed on each message, so only a message this
  listener deferred gives a slot back when it is redelivered; one arriving past that cap is given
  up at once. A slot whose message never comes back to this listener — the stream dropped or
  purged it, or another subscriber took its redelivery — expires 125 s after the deferral that
  took it (the five-second delay plus two of the durable's 60 s ack waits), so such messages hold
  the budget no longer than that. Each deferral logs `listener agent delivery deferred` at INFO
  with `reason`,
  `attempt` and `window_deferrals`, the deferrals that message's cause is allowed (1 for
  `push_failed`). The give-up counts its `delivery_status` and logs `listener agent delivery given
  up` at WARN first, and only then publishes the `no_holder` delivery exception, once. A notice
  that cannot be published — it embeds the original payload, so an envelope near the server's max
  payload has its notice refused as too large on every attempt — logs `listener exception publish
  failed` beside the give-up line and nothing else: the message is not NAK'd, it is acknowledged,
  and it keeps the attempt record that stops a redelivery from pushing again. The ambiguous
  `listener agent session not found anywhere`, which acknowledged such a message away on its first
  attempt and counted it `skipped`, is gone, and `envoy_messages_delivered_total` carries the new
  `delivery_status` labels `unplaceable_no_record`, `unplaceable_no_push_target` and
  `unplaceable_push_failed` for those give-ups. Every send and publish is recorded in one line,
  `listener send` or `listener publish`, INFO when accepted and WARN when refused, naming the
  route's destination (`target_session` or
  `topic`), the envelope's `event_id`, `source`, `source_session` and `payload_summary`, the
  `status` answered, the `holder` a role lane resolved, and an `outcome` of `accepted`,
  `invalid_request`, `too_large` or `bus_unavailable` on either line, `no_live_session` or
  `capability_refused` on a send, and `publish_denied`, `role_unclaimed`, `role_lapsed`,
  `role_holder_changed` or `role_claim_unreadable` on a publish (INFRA-141).
- The Envoy listener's webhook routes and `/healthz` answer a bus that went silent inside their
  callers' wait (INFRA-141). A GitHub check_suite
  or check_run delivery's CI record - the wait for a write it queues behind and its own write - is
  held to 4 s, two write budgets, each KV call included, where a silent bus held it for the 10 s
  JetStream wait, and ends sooner when its request does; it is still the 503 a failed record was.
  A write given up on may land after that 503, but it is a compare-and-swap at the revision it
  read, and a redelivery of its observations writes nothing. A Slack event's publishes end 2.5 s
  after the handler starts, inside the 3 s Slack waits, where they had the bus's 5 s; a Ghost
  Wispr event's and a GitHub event's end when the sender drops the request, or at that 5 s; each
  is still the 503 a failed publish was. `/healthz`'s flush, cache KV pings and durable consumer
  read share one 3 s budget, so a stall in JetStream alone, which the flush does not see, is
  answered in 3 s rather than 10 s: a check that did not answer within it is `503 unhealthy`
  naming the dependency and `context deadline exceeded`, so the container health check (`curl
  -sf`) still fails and replaces the task, as it did when the 10 s wait outran it. A check that
  answered with a failure keeps its answer.
- A session holds a role exactly when the role's claim in `envoy_roles` names it, and every
  answer that names a role's holder reads the claim (INFRA-141). `GET /v1/sessions` rows and `GET
  /v1/registry/<session_id>` (which Dispatch's route roster reads) report `roles` from the claims,
  and so does the `sender.roles` a send or a publish stamps, where both took them from the
  `notifications.role.<role>` topics on the session's interest record: a session whose record kept
  a role's topic after another session claimed the role was reported holding it, and a holder
  whose record lacked the topic was not. The listener keeps a watched view of the role bucket
  (`store.Registry.RolesHeldBy`), so no answer pays a store read per role; a claim it cannot decode
  gives its role to nobody and is logged at WARN once. The view answers only while it is complete:
  its watcher's first scan delivered every claim, and after a recreated role bucket's reset only
  once the refill's scan has. Until then (still loading, a scan nats.go's idle timer ended, a
  refill still running, or its watcher ended) it is `store.ErrRoleViewNotReady`, a read that did
  not answer: the bus-unavailable 503 with `Retry-After: 1` on the two session routes, never a
  holder listed without its role, and a send's `sender.roles` left out without refusing it. A
  first scan the idle timer ended is scanned again a second later until one completes
  (`kvwatch.WithRescan`). The role claim watcher is the interest registry's second, so `/healthz`
  now answers `503 unhealthy` when it stops or the role bucket is recreated under it
  (`interest KV watcher unavailable: role claims watcher stopped`,
  `interest KV watcher unavailable: role claims bucket envoy_roles was recreated under its watcher`,
  or `interest KV unavailable: ...` from the request whose own ping finds the recreate), where a
  ping of the role bucket's status was all it read before, and the self-health monitor counts
  either as a terminal failure and rebuilds it, which refills the view. `POST /v1/roles/set`'s
  rollback of a claim that did not land and its removal of the role's topic from the old holder
  are best-effort tidying within the request's budget: a claim that landed answers 200 whatever
  that cleanup did, a cleanup that failed is logged at WARN, and one the budget's end cut short at
  INFO. A claim whose lost compare-and-swap is joined with a store call that did not answer is the
  bus-unavailable 503 with `Retry-After: 1`, not the store's answer. The start's warm-up wait
  covers the role claim view too, and its timeout is logged
  `interest or role claim cache warm-up timed out`, its error naming the cache.
- A comment, comment edit, message or broadcast body is at most 32,000 characters
  (`contracts.DispatchBodyMax`, UTF-16 units), up from 2,000 (INFRA-45); a longer one is
  `400 CAP_EXCEEDED` as before, named the way every other cap names its own. The limit is
  generated from `DISPATCH_BODY_MAX` in `packages/contracts`, which also generates the Legion
  daemon's `dispatch.MessageBodyLimit`, so that daemon's derived `record.MessagePostLimit` moves
  from 1,956 to 31,956. A comment anchored to a document is also projected into that document's
  margin, whose budgets are in bytes and do not move with this change: 256 KiB of text per margin
  record — a thread's comment, its replies and its suggestion's replacement — and 1 MiB per
  document margin, refused `413 CAP_EXCEEDED`. A body at the new cap is 32,000 bytes of ASCII and
  up to 96,000 bytes of three-byte text, so one thread record now holds roughly eight full-length
  comments and fewer in CJK, and a document keeps every comment it has had, so one whose margin is
  full takes more once it is split.
  **Rollout:** deploy this Dispatch server before any client that writes a
  longer body, and in particular before a Legion daemon of this release. A server still on 2,000
  refuses a body a new client sends, and both skews are real. An over-long `dispatch_message`
  outbox row is not marked permanently refused: it retries forever at a backoff capped at 64 s,
  costing one retrying logged row per over-long post and blocking nothing else. Symmetrically, a
  new `dispatch` CLI, plugin bundle or worker image against an old Dispatch lets an agent compose
  up to 32,000 characters and take a `400 CAP_EXCEEDED` from the server. No code gates the order.
- **Breaking:** deploy `0113` with no server of the earlier release running, and never start one
  again after it applies. Stop it, or scale it to zero, before this release boots (a Recreate
  rollout, never a rolling one), and recover a failed deploy forward rather than by rolling the
  image back. Only this release's API holds the depth limit, so an earlier release serving over
  `0113` can build a deeper chain again, and every read of that chain's issues fails (500) until an
  ancestor that still reads has its parent set to null.
- **Breaking:** migration `0117` is a stop-the-world migration. Deploy it with no server of the
  earlier release running, and never start a release older than `0117` over it again: stop the
  earlier release, or scale it to zero, before this release boots (a Recreate rollout, never a
  rolling one), and recover a failed deploy forward rather than by rolling the image back. Over
  the new schema an earlier release's architecture-source save answers `500`, since no unique
  index covers `project_key` alone; its import deletes every component of the project before
  inserting one source's, so a model with components fails on the new `source_id` column and one
  with none deletes every source's components; and its delete removes every source of the
  project. Its census counts components whose project has no architecture source, which only
  direct SQL writes; repair them on the release still serving with
  `delete from components c where not exists (select 1 from architecture_sources s where s.project_key = c.project_key)`
  and take the census again (`cmd/dispatch/README.md`, "Database migrations").
- **Breaking:** the singular architecture source routes `PUT` and `DELETE
  /api/v1/projects/{key}/architecture-source` are gone. A source is addressed by its repository
  under `/api/v1/projects/{key}/architecture-sources/{owner}/{repo}`, and a project's sources are
  listed at `/api/v1/projects/{key}/architecture-sources`; the `dispatch` CLI of this release uses
  the new routes. Two singular routes are kept for `dispatch` clients older than this release,
  each call logged at warn naming the route and the project:
  `GET /api/v1/projects/{key}/architecture-source`, which those clients read on every issue
  create, answers `200 null` when the project has no source, otherwise its first source's row,
  `404` for an unknown project, and the list route replaces it;
  `POST /api/v1/projects/{key}/architecture-source/sync`, which their `dispatch architecture-sync`
  calls, imports every source as `POST …/architecture-sources/sync` does and answers one row: the
  first source in the order they were added whose `last_error` is set, or the first source when
  none failed. A project with no source is still `404 SOURCE_NOT_FOUND`, and a failure that could
  not be recorded `500`; a credential or branch problem, once a `409 SOURCE_ACCESS`, is recorded
  on its row and answers `200` with that `last_error`, which those clients print as
  `Sync failed for …`. `POST …/architecture-sources/sync` replaces it.
- **Breaking:** the Dispatch auto-deployer (`deploy/scripts/autodeploy.sh`) accepts `DATABASE_URL`
  in one form only, `postgres[ql]://<user>[:<password>]@<host>[:<port>]/<database>`, with an
  optional query of `sslmode`, `connect_timeout` and `application_name`.
  - The user, database and query values are ASCII letters, digits, `.`, `_` and `-`.
  - The host is dot-separated labels of ASCII letters, digits and hyphens: a host name or an IPv4
    address.
  - The port is 1 to 65535.
  - The password is ASCII unreserved characters and percent-escapes, so its `@ : / ? # [ ]`, `%`, a
    space and any other character must be percent-encoded. `%00` is refused, and the decoded
    password must be valid UTF-8.

  The form is checked with ASCII ranges whatever the locale. Every other form is refused before
  anything runs: a keyword/value string, a bracketed, IPv6, multi-host or socket host, any other
  query key, or a password in the query. The refusal points to `deploy/README.md` and never prints
  the URL. In that form the deployer, libpq and pgx all read the password from the same place, so
  its `pg_dump` and `psql` get the URL without the password as their argument and the password as
  `PGPASSWORD` alone. They run under `docker run --init` so a timeout ends them, and `psql`'s
  stderr, which can quote the connection, is dropped.
- The Dispatch auto-deployer no longer rolls a failed deploy back to the previous image once that
  deploy has moved the database's schema. It records the schema version the deployed tag runs
  against (`ENVOY_IMAGE_SCHEMA` in `compose/.env`) before it deploys. When a deploy fails, it stops
  the failed image, confirms no `dispatch` container still runs, and reads the version again behind
  the migration runner's advisory lock. If the stop or the read fails, or the schema moved past the
  record, it starts nothing, logs `refusing to roll back` with both versions, and exits 1, leaving
  the recovery to an operator, forward. It also stops before a deploy whose schema version it
  cannot read, and a failed pull no longer restarts the service. CI runs its test
  (`deploy/scripts/autodeploy_test.sh`) in the `envoy-go` job.
- An unauthenticated listener (neither `ENVOY_API_TOKEN` nor the OIDC pair) serves `/v1` only to
  a request whose `Host`, port stripped, ASCII case folded and one trailing dot dropped, is `localhost`,
  a loopback address (`127.0.0.0/8`, `::1`) or a name in the new `ENVOY_ALLOWED_HOSTS`
  (comma-separated names or addresses, no port; an entry that is not a host name or an address,
  such as one with a port, scheme or path, refuses startup naming it).
  Any other `Host`, an empty one included, is answered 421 with a JSON error naming it, so a
  DNS-rebinding page can no longer drive the bearer-free API through a loopback port or a forward
  that reaches it. A listener that requires a bearer checks no `Host`. `/healthz`, `/metrics` and
  the webhook routes are unaffected. An unauthenticated listener clients reach by another name (a
  tailnet forward to it) must list that name in `ENVOY_ALLOWED_HOSTS`, or those clients get the 421.
- The secrets broker's policy keeps a single-name reread against a lagging listing only when the
  reread changed the live answer; a reread that confirms what the policy serves costs the next
  reloads nothing. A grant whose frozen source is no longer the one the policy serves its secret
  from (a secret deleted and recreated under its name on AWS, or a move between stores) answers
  `403 GRANT_NOT_LIVE` and is not reused, where it answered `404 SECRET_NOT_IN_STORE`; the session
  asks again.

- **Breaking:** every listener `/v1` request with a body must set `Content-Type: application/json`;
  parameters such as `charset=utf-8` are accepted. Any other type, a malformed one, or none is
  answered 415 with a JSON error before authentication or the handler runs, whether the body's
  length is known or not (a chunked body is checked too). A cross-site browser request can no longer
  publish a `text/plain` body through an unauthenticated loopback listener. A request with no body,
  such as a `GET` or `DELETE`, is unaffected, and webhook routes keep their own signature and
  content handling. Every Legion client already sends the header; an operator's own script that
  posts to `/v1` without it now gets the 415 and must set the header.

- **Breaking:** `GET` and `PUT /api/v1/settings/delivery` replace
  `production_job_name` with `production_job_names` (a string array) and
  `production_apply_step_name`. `PUT` trims each name and refuses an empty list, a blank name or a
  duplicate with `DELIVERY_SETTINGS_INPUT`. Migrations 0109 and 0110 retain the old database column
  through one rollout while recording the configured apply step and its conclusion for listed jobs.
  A production job that succeeds without that step succeeding does not count as a deploy. Warnings
  name every configured production job and a missing apply step, and the Pipeline card's `job`
  joins the names it follows with `or`.

- **Breaking:** migration 0111 drops `delivery_settings.production_job_name`, with the trigger and
  function 0109 added to keep it at the first of `production_job_names` through the rollout
  (LEGION-761). No value is lost: the trigger kept the column equal to the list's first name, and
  the migration's census counts any row where it did not, so such a row refuses the deploy.
  Rollback floor: once 0111 has applied, a task older than legion-envoy v13.4.1, the release that
  added 0109, still boots, but its reads and writes of the delivery settings fail with SQLSTATE
  42703 (undefined column), since they name `production_job_name`, and so does every delivery page
  and job that reads the settings first.
  v13.4.1 and every later release read `production_job_names` alone.

- **Breaking:** `envoy-dispatch` and `natstail` read `envoy.json` from
  `${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json` and the `dispatch` section of the checkout's
  `.envoy/envoy.json`, and take the NATS URLs from `envoy.natsUrls` (LEGION-707). A checkout's file
  that holds an `envoy` section refuses startup, naming `envoy` and the user file it belongs in
  (`envoy: not read from a checkout's envoy.json; put it in <user file>`): a cloned repository
  does not choose the NATS servers the process connects to. The session clients ignore such a
  section and say so in their start notice instead. A `.envoy` that is a plain file, as some
  repositories have, counts as no checkout file. A JSON `null`, at the top level, as a section or
  as any key's value, is refused naming the key (`dispatch: Expected object`,
  `envoy.natsUrls: Expected string array`), as the clients refuse it, and JSON that does not parse
  is refused naming the file, the line and the column, never the text it stopped at, which can be
  the start of a token or seed. A top-level `natsUrls` is refused: a user file's naming
  `envoy.natsUrls`, a checkout's saying a checkout's envoy.json holds only a `dispatch` section.
  `~/.config/opencode/envoy.json` is not read: while it is the only user file and the user file is
  `~/.config/legion/envoy.json`, startup refuses, naming both paths and the move
  (`<old file> is not read: mkdir -p <user file's directory> && mv <old file> <user file>`), whatever
  `DISPATCH_SERVER_URL` and `NATS_URLS` say. A checkout's `.opencode/envoy.json` is not read: while
  the checkout has no `.envoy/envoy.json`, `envoy-dispatch` logs one WARN at start
  (`dispatch: a checkout's .opencode/envoy.json is not read`, with the rename as `fix`) and
  `natstail` prints the rename. Neither gives a `mv` across a mount: where the old file's directory
  and the nearest existing ancestor of the new file's directory are on different filesystems (as in
  an agent box that bind-mounts `~/.config/opencode`), the text is `<old file> is not read, and is
  on another filesystem than <new file> (a mount): create <new file>, and move <old file> on the
  machine that owns it` (`config.LoadOptions.DeviceOf` answers a directory's device, `stat`'s by
  default). The `envoy` section takes `enabled`, `url`, `natsUrls`, `token` and
  `natsSeed` and refuses any other key; the server reads `envoy.natsUrls` whatever
  `envoy.enabled` says, since `enabled` switches a session's Envoy. `XDG_CONFIG_HOME` is a row of
  the settings table, and one set to a path that is not absolute refuses startup, naming it.
  `DISPATCH_SERVER_URL` and `NATS_URLS` still override both files, but not the old-file refusal.
  No Go process runs `envoy.natsSeed`: `natstail` and `envoy-dispatch redeliver-webhooks` connect
  as the nkey user `NATS_NKEY_SEED_FILE` (or `NATS_NKEY_SEED`) names, so a run against a NATS that
  requires a user needs `NATS_NKEY_SEED_FILE`, which the session launchers no longer export.
  The compose deployment (`deploy/compose/dispatch.compose.yml`) mounts no `envoy.json` and needs
  no change.
- **Breaking:** the secrets broker refuses to start when `BROKER_SERVICES` binds a namespace's
  `default` service account (`system:serviceaccount:<namespace>:default`), naming the entry: every
  pod in that namespace whose spec names no account runs as it and can get a token for the broker's
  audience, so the binding proves nothing about which pods are the service's. Bind each service to
  a dedicated account its pods set as `serviceAccountName` before taking this release.
- `BROKER_SERVICES` may name one service for several accounts, one entry each, and a pod running
  as any of them is that service's own session. Each account still proves one service, and an
  account given twice is still refused, now naming the account and its one service when both
  entries name the same service. Rollback: a broker before this release refuses a
  `BROKER_SERVICES` that names a service twice at start-up (`must give each service once`), and
  the broker is one pod, so roll the image back only together with the setting.
- **Breaking:** a renewal (`POST /v1/enrollments/{id}/renew`) of an enrollment that has ended,
  lapsed or never existed answers `401 ENROLLMENT_NOT_LIVE` where it answered `401 PROOF_INVALID`;
  the renew route alone answers it, and every other session route still answers a not-live proof
  `PROOF_INVALID`, the code that stays for retryable proof failures such as iat skew. A pod's
  renewal carries the pod's current projected token as `pod_token`, and the broker refuses it
  `403 POD_IDENTITY_MISMATCH` unless the token names the pod UID and service account the enrollment
  was made for, and `403 POD_TOKEN_EXPIRED` (naming its expiry, and retried by the renewer with the
  token its file holds next) once it has expired. A machine session's renewal carrying a token is
  `403 POD_IDENTITY_MISMATCH`. The token is not yet required, and the broker ratchets each pod UID
  instead: every pod enrollment, new or found by migration 0014, starts unmarked, and the first
  renewal that carries the pod's token marks it. Once any enrollment naming a pod UID, live or ended, is
  marked, a renewal without the token from any enrollment naming that pod, whichever machine login
  made it, is `403 POD_TOKEN_REQUIRED`; until then it is accepted and logged as `broker: pod
  enrollment renewed without its projected token`. So the copied-token gap closes for every pod UID
  a current-image pod has renewed under, and stays open for old-image UIDs until the release that
  requires the token. A current-image pod still leaves one window: a copied token used before the
  pod's own first renewal with its token. That is seconds while the Legion daemon holds an approved
  machine login; after a daemon restart its pods enroll only once a person approves its new login,
  and a pod that ends before it enrolls never renews with its token, so its UID stays open as an
  old-image pod's does. A pod UID names one pod for ever, so a mark refuses nothing of another pod,
  and it keeps refusing a copy between the pod's role generations and after the pod is gone. The
  renewer stops on `POD_TOKEN_REQUIRED` and `ENROLLMENT_NOT_LIVE` and
  retries `POD_IDENTITY_MISMATCH`, which the broker also answers for a token it could not verify
  while its issuer was unreachable; a real mismatch ends with the lease. `agent-secrets renew
  --pod-token-file FILE` reads the file before every renewal and sends it, and Legion's worker shim
  passes the projected token file. **Rollout:** any order keeps every pod renewing (an older
  broker reads no renew body, and this broker accepts a token-less renewal from a pod no
  enrollment of which has sent one), but it does not bound exposure: a grant lasts until its
  session ends, so a copy renewing an old-image pod's UID keeps one approval's grant for as long as
  it renews. Before or right after the broker deploys, replace every old-image issue pod, then
  revoke the machine login that made each enrollment a remaining token-less WARN line names (the
  line's own enrollment) on Dispatch's machine-login page, by its **Credential** id once its code
  shows it is not the daemon's, never by its machine name, which a copy's login can share (the
  operate page gives the query). The release that requires the token is safe once all three hold:
  `select count(*) from enrollments where kind='pod' and revoked_at is null and pod_token_renewed_at is null and created_at < now() - interval '1 minute'`
  answers 0; the broker has logged no token-less WARN line for one full `BROKER_LEASE_SECONDS`; and
  no issue pod runs a worker image older than the one whose renewer sends the token (an old-image
  issue pod between role generations holds no enrollment, so the count and the log miss it, yet its
  next phase enrolls an unmarked row).
- **Breaking:** only the machine login that enrolled a pod may end its enrollment:
  `DELETE /v1/enrollments/{id}` of a pod enrollment by any other login, another service's or a
  second login of the same service, is `403 NOT_ENROLLING_CREDENTIAL` and ends nothing. A Legion
  daemon that logs in again (after a restart, or when its login reaches its expiry,
  `BROKER_LAUNCHER_CREDENTIAL_SECONDS`, 7 days by default) cannot end its earlier login's pods;
  each ends one lease after its pod stops renewing, or at once when someone revokes that earlier
  login, and the daemon no longer retries that refusal.
- `agent-secrets renew --pod-token-file FILE` re-reads the projected pod token before each renewal
  and sends the token as `pod_token`. A blank or unreadable file posts no renewal: the renewer
  prints `ERROR: enrollment <id> cannot read its pod token (<reason>); nothing was sent, so its
  lease lapses unless the file becomes readable` and retries rather than exiting 76 (at start it
  exits 2 and the shim starts it again). The worker shim passes its projected token file. This
  client works against a broker that predates the renewal body because that handler ignores it.
  The terminal renewal refusals `POD_TOKEN_REQUIRED` and `ENROLLMENT_NOT_LIVE`, and an older
  broker's `PROOF_INVALID` saying `not live`, exit 76 (`EX_PROTOCOL`) naming the enrollment, HTTP
  status and broker code; the shim quotes that terminal line beside its own enrollment and stops.
  Every other refusal retries, `POD_IDENTITY_MISMATCH` among them, which the broker also answers
  for a token it could not verify while its issuer was unreachable.
- `GET /api/v1/references` no longer renders every document that mentions its target to cut each
  backlink's one-block excerpt (LEGION-531; 24.6% of a week's production CPU). Each document
  mention's excerpt - the block holding the target's first mention, its `#b-<id>` jump and its
  markdown cut to 480 characters - is stored on its `refs` row when the mention is indexed, cut
  from the render the version write already makes, so the route reads one indexed query and loads
  no document. Migration `0092_refs_excerpts` adds the three nullable columns (`excerpt_block_id`,
  `excerpt_text`, `excerpt_version`) and the partial index `refs_excerpt_drift`; its census answers
  `0`. `envoy-dispatch` runs a reconciliation that fills every row indexed before it, one batch of
  20 documents per tick. Until it reaches a row, the route answers that mention with the
  document's name, the fallback a mention in no block already got. The excerpts the dashboard
  shows are unchanged: on the 2026-10-10 production dump, all 7,537 document mentions match the
  rendered path. In the rolling deploy that ships 0092, a version the outgoing task writes keeps
  the previous version's excerpt until the reconciliation's next tick, within five minutes.
- **Breaking:** an agent session on a machine its person logged in is one enrollment kind,
  `session`, where the broker recorded `host` for a session the helper enrolled for itself and `box`
  for a key a launcher such as agentbox brought; a pod stays `pod`. Migration
  `0012_one_session_kind` rewrites every stored `host` and `box` row to `session` and allows only
  `session` and `pod`. Every place the kind is shown says `session`: `GET /v1/enrollments/self`,
  the grant and record reads (`agent-secrets self`, `grant list`, Dispatch's credential page and
  Live grants), and a new record's enrollment line, while a record written before keeps its body,
  which its id hashes, verbatim. A reader matching `host` or `box` must match `session`. The
  clients in the field keep working through the rollout in either order: the broker records an
  older helper's `host` or `box` as `session` and logs `an enrollment named a legacy kind`; this
  helper asks an older broker, which refuses `session` with `400 INVALID_KIND`, once more as
  `host` or `box` (the broker's half is removed since: it now refuses `host` and `box` with
  `400 INVALID_KIND`). The broker is forward-only from this migration: a broker before it inserts
  `host` and `box`, which the new constraint refuses, so roll forward rather than back.
- **Breaking:** a grant lasts until its session ends unless its request asked for a shorter
  lifetime (`--ttl`, above); there is no fixed lifetime and no cap. `BROKER_MAX_GRANT_SECONDS`
  (default and most 43200, 12 hours) is removed: a deployment that still sets it refuses to start,
  naming the variable and explaining that a request's `--ttl` now chooses a shorter lifetime. A
  grant with no lifetime answers `expires_at: null` on every
  grant read (`GET /v1/enrollments/self`, `GET /v1/grants`, `GET /v1/operator/grants`), and its
  record reads `lifetime_seconds: null` on `GET /v1/credential-requests/{id}` and
  `lifetime_seconds: -` in its body. Migration `0013_grant_until_session_ends` drops not null from
  `requests.lifetime_seconds` and `grants.expires_at`, and every liveness check reads a null expiry
  as one that has not passed. `agent-secrets self` prints `until this session ends` for such a
  grant and `grant list` prints `session-end`; an `agent-secrets` from before this release prints
  `0001-01-01T00:00:00Z` for it, and nothing else changes for that client. Dispatch's credential
  page shows **Until the session ends** and Live grants **When the session ends**. A broker before
  this migration cannot read a grant with no expiry, so roll forward rather than back.
  A running session keeps what it was given across the deploy: a request without a lifetime takes
  the live grant of a request written before this migration (which recorded the old fixed 43200
  seconds), keeping its expiry, and joins such a request while it is still pending. Only rows
  written before migration 13 was applied match, so a later `--ttl 12h` request is never taken for
  one, and nothing matches a day after the deploy.
- **Breaking:** a session has at most one pending request for each secret. `POST /v1/requests`
  answers a request naming a secret one of the session's pending requests names, with another set of
  names or another lifetime, `409 REQUEST_PENDING`, naming the pending request; `agent-secrets`
  exits 1 with that message. This holds for requests without `--ttl` too: a request for `A` and `B`
  while one for `A` waits used to open a second record for the approver, and is now refused. The
  identical request (the same names and the same lifetime, or none) still joins the pending one,
  answered with that request and `coalesced: true`. Wait for the named request to be decided, or
  `agent-secrets cancel` it and ask again.
- The helper's `enroll-box` and `unenroll-box` ops are `enroll-session-key` and
  `unenroll-session-key`, which name no kind, and `agent-secrets enroll --helper` needs no `--kind`.
  Every older client keeps working: the helper still answers `enroll-box` and `unenroll-box`, from
  an `agent-secrets` before this release, as the new pair, logging `a client asked a legacy op`;
  this `agent-secrets` asks a helper before this release, which answers
  `unknown op enroll-session-key`, its `enroll-box` and `unenroll-box` instead, saying so on
  stderr; and `enroll --helper --kind box`, which dotfiles' agentbox passes, enrolls as before
  until dotfiles pins this release and agentbox stops passing it, while any other `--kind` exits 2
  naming the flag, as every `--kind` but `box` did before. An `enroll` without `--helper` is
  refused for that first, whatever `--kind` it carries, as before. (Superseded by the Breaking
  entry on `enroll-box`, `unenroll-box` and `--kind` under Changed: those fallbacks are removed.)
- `POST /v1/enrollments` answers a pod token whose expiry has passed with `403 POD_TOKEN_EXPIRED`,
  naming when it expired (`the projected token has expired (oidc: token expired at <time>)`).
  Before, the broker answered it as `403 POD_IDENTITY_MISMATCH`, "the projected token does not
  identify this pod", which reads as another pod's token. `POD_IDENTITY_MISMATCH` now means only a
  token that does not prove the pod. A launcher that branches on the code presents the pod's
  current token with the same key; the Legion daemon reads the new code as not permanent, keeps
  the key, and enrolls it with the pod's current token, which it asks the shim for at its next
  observation.
- **Breaking:** `agent-secrets launcher login` and `launcher login-status` are removed, with no
  alias: `agent-secrets machine login` and `machine login-status` replace them, beside
  `machine list` and `machine revoke`. A script that still calls `launcher` gets the usage and
  exit 2, so a session launcher gating on `launcher login-status` reads that as no login and starts
  its sessions with no broker identity. Move every caller to `machine login` and
  `machine login-status` before it pins this release. Every message that said to run
  `agent-secrets launcher login`, from the CLI and from the helper, now names `machine login`.
- The secrets broker takes no database password apart from its URL, and refuses to start while
  `BROKER_DATABASE_PASSWORD` is set or `BROKER_DATABASE_URL` names its
  `${BROKER_DATABASE_PASSWORD}` placeholder, naming the variable and why: on Amazon RDS it signs in
  by IAM token, and any other database's password goes in the URL itself, URL-escaped. A deployment
  that still sets either must stop before it runs this broker: put the password in the URL, or move
  to an IAM-form URL on RDS. Going back to the password once a broker runs this release takes the
  reverse order, since a broker on this release exits 1 at boot while either is set: roll the image
  back to legion-envoy v7.6.0 or later, the 7.x releases that sign in by IAM token, which hold every
  broker migration to date and still accept the variable. Once the Legion daemon sends no
  `login_hint` (sjawhar/legion#1870), roll back to v7.10.0 or later instead: earlier brokers need
  that hint on a service's machine login and answer the daemon's next fresh login with a 500. Then
  restore the variable and the `${BROKER_DATABASE_PASSWORD}` placeholder in the URL together.
  Undoing the configuration change that moved a deployment off the variable therefore brings no
  broker up on this release. Before going back, check that the RDS master user does not hold
  `rds_iam`, directly or through a role (if it does, its password sign-in is refused), and that its
  secret's value is current. When IAM sign-in itself is what is broken, make the rollback one
  change: the image and the variable in one task-definition revision, so no broker boots on this
  release with the variable or on 7.x without a working sign-in.
- Anyone signed in to Dispatch decides a service's machine login, such as the Legion daemon's, not
  only the person its request names: the broker reads the service from the signed request, opens
  its record with the approver `anyone` and ignores any `login_hint` it carries, and the decision,
  the credential's chain re-check, the machine-login list and its revoke all follow that service,
  so a login a daemon starts while still naming one person is anyone's to decide, and so is one a
  broker before this change opened naming a person. The pending list keys on the record's stored
  approver, so it shows every new service login to everyone, but a service login an older broker
  opened naming a person is listed for that person alone, though anyone who types its code may
  decide it. Every signed-in
  person lists and revokes every service's login, each row naming who approved it (`approved_by`),
  and Dispatch's machine-login page calls the list **Machine logins**. A person's own machine login
  is unchanged: only the person it names decides it, and a person's login naming no one, or
  `anyone`, is refused `400 REQUEST_INVALID` before any record opens. Every service's login spends
  one shared rate-limit bucket, whatever service it names, and a person's machine login the bucket
  of the person it names (`service`, `person:<login>`), so an invented service name buys no bucket
  of its own (LEGION-664).

- An upload's kind is read from its bytes, never from the type its client declares: it is an
  `image` only when `http.DetectContentType` reads its bytes as a PNG, JPEG, GIF or WebP, the
  pictures a model is shown, and its `mime` is then the type its bytes are, whatever was declared
  (a JPEG declared `image/png` is stored and served as `image/jpeg`; a PNG declared
  `application/octet-stream` is an image). Any other upload but a `text/markdown` document is a
  `file` under its declared type, an SVG among them, so a client can no longer store arbitrary
  bytes as an image. This holds for an issue's, a project's and an agent's conversation's uploads
  alike. Stored artifacts keep their kind, so a new version of an `image` whose bytes are no
  picture is `400 ARTIFACT_KIND_MISMATCH` (LEGION-541).
- A new upload's slug keeps only the ASCII letters and digits of its lowercased name, each other
  run one dash (`café.png` is `caf-png`, `スクリーンショット.png` is `png`, a name with neither is
  `artifact`), for an issue's, a project's and an agent's conversation's uploads alike: every
  grammar that reads a slug back (the server's text references, the dashboard's routes, 0009's
  cleanup) is `[a-z0-9]+(-[a-z0-9]+)*`, so a slug holding another letter was stored but named
  nothing, and a picture pasted under such a name showed as a broken image (LEGION-541). Existing
  slugs are kept.
- `GET /api/v1/search` ranks each kind of content (issues, documents, asks, comments, messages) on
  a list of its own and merges the lists by reciprocal rank fusion (each hit scores 1/(60 + its
  position in its list), which is now its `rank`), so a page takes each kind's best in turn where
  it used to fill with long documents that repeat the query's words. Hits that score alike are
  ordered issue, document, ask, comment, message, and an issue whose key is the whole query heads
  the issue list (LEGION-386). The answer carries `total` (every match), `reachable` (the hits the
  pages can return, each kind's best `contracts.SearchKindDepth`, 100), `limit` and `offset`, and
  takes `offset` (0 or more; anything else is `400 INVALID_OFFSET`), so a query with more matches
  than one page says so and the rest can be paged to (LEGION-382).
  `TestSearchLatencyOnCorpus` no longer fails at an absolute 100 ms: production's `took_ms` already
  exceeds it, so `scripts/search-latency-compare.sh` runs it from a base checkout and this one on
  the same corpus copy and fails a median p95 more than 10% above the base's. `scripts/corpus-copy.sh`
  restores the newest nightly dump into a local Postgres, and `scripts/search-smoke.sh` prints any
  build's top hits for a list of queries.
- Event-log payloads served by `GET /api/v1/issues/{key}/events`, `GET /api/v1/artifacts/{id}/events`
  and the replay from `GET /api/v1/events` keep their stored numbers and PostgreSQL `jsonb` object
  order: `9007199254740993` stays that integer, `1.00` keeps its trailing zeros, and object keys come
  shorter first and, for keys of one length, in byte order. The JSON is compact, and Go's encoder
  still writes `<`, `>`, `&`, U+2028 and U+2029 inside strings as `\u` escapes, as it did before.
  Six ask payloads are decoded to add read-time fields and come back in Go's sorted-key, `float64`
  form: `ask.opened`, `ask.anchor_refreshed`, `ask.answered`, `ask.resolved`, `ask.edited` and
  `ask.handed_back`; `ask.follower_added` and `ask.follower_removed` keep the stored order. A live
  event on `GET /api/v1/events` keeps its producer's field order. The dashboard, the agent Dispatch
  tools and the Legion daemon read named fields from these payloads, and none compares payload bytes
  or relies on Go's map ordering or `float64` rounding.
- People sign in to Dispatch with Google Workspace through the shared sign-in pool (OpenID
  Connect authorization code against `DISPATCH_SIGNIN_ISSUER`, with `DISPATCH_SIGNIN_CLIENT_ID`,
  `DISPATCH_SIGNIN_CLIENT_SECRET` and `DISPATCH_SIGNIN_GROUP`, all four required by cookie
  identity) and are named by lowercase email everywhere: the email in their pool username, never
  the `email` claim. Only a member of `DISPATCH_SIGNIN_GROUP` signs in, and Dispatch confirms the
  membership with the pool's refresh token at least hourly, ending every session of a person the
  pool refuses or drops from the group; the event streams and the document websocket re-check the
  caller on every heartbeat. The session cookie's payload encodes the email so any address
  round-trips; a cookie of the earlier shape no longer verifies. Header identity is only for tests
  and local harnesses, requires `DISPATCH_IDENTITY_HEADER_TRUSTED=1`, and never shares a
  deployment with the sign-in pool. Everyone who has signed in is recorded in `people` (migration
  `0068`), which is the assignee picker's list and the only names an issue may be assigned to. The
  GitHub reads (`/api/github/rest/...`) go to GitHub as the GitHub App installation, GET only and
  only a pull request, an issue or a commit's check runs. `DISPATCH_ALLOWED_LOGINS` and
  `DISPATCH_APP_CLIENT_SECRET` are removed and refused at boot; GitHub OAuth sign-in, the per-user
  GitHub token table (`users`, dropped by `0068`) and the GraphQL proxy are gone.
- Dispatch stores each person's sign-in pool refresh token sealed under a key derived from
  `DISPATCH_SIGNING_KEY` (`people.refresh_token`, a `v1:` format; `cmd/dispatch/README.md`,
  Identity). A stored value that does not open counts as no refresh token: each read logs
  `dispatch: a stored refresh token did not open; the person signs in again` at WARN without the
  value, and the person signs in again. Every boot retires each refresh token stored in plain
  text, by `0068`'s release or by one of its tasks during a roll or after a rollback: it revokes
  the token at the pool's discovered `revocation_endpoint`, then clears it; a sign-in that replaces
  one retires it first. A token the boot cannot revoke stays for the next boot and is logged at
  ERROR with the email and the error, never the token; a sign-in that cannot revoke the token it
  replaces goes on and logs its own ERROR line, `dispatch: a sign-in replaced a refresh token the
  pool did not revoke; sign this person out at the pool`. A redirect from the revocation endpoint
  is a failure, never followed. The boot logs how many it retired and how many failed, and stops
  after 30 seconds without holding up the start. A database backup holds no refresh token the
  pool would accept only once a boot of this release has retired every plain-text token, and then
  only for the tokens still in `people` when it ran: one `0068`'s release removed without revoking
  (a sign-in that replaced it, or a logout) stays valid at the pool in any backup that holds it,
  so delete every backup taken between `0068`'s deploy and the post-roll restart, or sign out at
  the pool everyone whose `people.signed_in_at` is after `0068`'s deploy. After the last
  earlier-release task is gone, restart Dispatch once, then check that boot logged `failed=0`, no
  sign-in-replaced line appeared, and
  `select count(*) from people where refresh_token is not null and refresh_token not like 'v1:%'`
  answers `0` (`cmd/dispatch/README.md`). Rotating `DISPATCH_SIGNING_KEY` leaves Dispatch unable
  to open any stored refresh token, so everyone signs in again, but revokes none at the pool:
  after a key leak, sign people out at the pool.
- A blank approval-request `summary` is refused (`400 SUMMARY_INPUT`) with text that asks for what
  the human is approving, rather than for what the version proposes that the human has not agreed
  to, and the advice in `409 APPROVAL_WAITS_ON_HUMAN` and in an approval ask's `409 ASK_KIND_FIXED`
  says to hand the request back only once the human has agreed to every point in the document
  (LEGION-475).
- A Dispatch approval request follows its document's versions instead of being retracted and
  reopened on every edit (LEGION-470). A version write moves the open request to the new version
  (`ask.edited`), keeping its thread and summary, and leaves it Waiting on agents until its agent
  calls `POST /api/v1/artifacts/{id}/approval-requests` again. Only the first move since the
  request was opened or handed back wakes its asker and followers, even when a reply in its thread
  had already left it waiting on its agent; each later move, while `requested_version` is already
  below the version it named, is recorded quiet (`quiet: true` on its `ask.edited`,
  `notify: false`, no follower route), as a human's unnamed version is, so a person typing in a
  document with a request open sends one delivery, not one per settled version. While the request
  waits on its agent (moved, or answered in its thread), that call hands it back to the human with
  the new event `ask.handed_back`, rewording it first when the summary is new (`ask.edited`); a
  hand-back that rewords nothing leaves the question and `edited_at` alone, so an answer the human
  had started is still saved. While the request already waits on the human, the same summary is a
  repeat that writes nothing and a different one is refused `409 APPROVAL_WAITS_ON_HUMAN`, since an
  approval request carries nothing new. The route answers 201 when it wrote anything and 200 when
  the request already stood as asked. A summary's limit is counted against a ten-digit version, so
  no version move takes the question past the ask cap. `ask.approval` gains `requested_version`, the
  version last handed to the human, and a document's `approval` gains `waiting_on` while it is
  `awaiting`. Every route that writes a comment (both comment routes and the delivery callback
  `POST /api/v1/comments/{id}/reply`) answers `ask_waiting_on`, the value the comment's event
  already carries, beside the comment on a reply to an open ask; a replayed delivery callback,
  which writes nothing, answers the stored reply alone. Migration `0064`
  backfills `requested_version` and adds `asks.handed_back_reply_id`, `0065` makes
  `comments.created_at` default to `clock_timestamp()` so an ask's newest reply is the one that
  committed last, and `0066` adds the column's foreign key to `comments` under a 500 ms
  `lock_timeout` of its own; each census answers `0`. The pre-deploy census judges a pending
  migration's lock holders against the `lock_timeout` the migration itself sets, so an autovacuum
  on `asks` or `comments` refuses `0066` where `deadlock_timeout` is not shorter than its 500 ms.
- A markdown document uploaded as an artifact is at most 1 MiB, the bound an issue's spec and
  every edit already have; other artifacts keep the 25 MiB limit. The dashboard shows the
  server's message for a refused upload (LEGION-465).
- The stream stores an envelope under a MsgId, and so recognises its repeat, when its dedupe key was
  minted once for its message: the listener's own `publish.<id>` and `agent.<session>.<id>`, and the
  same around the shared transport's UUID idempotency key (`contracts.MintedDedupeKeyPattern`,
  generated from `MINTED_DEDUPE_KEY_PATTERN` in `packages/contracts`). Only a re-send of that
  message repeats such a key: the transport's retry of a send whose answer was lost, which now
  answers `duplicate: true` and is stored once, and the Legion daemon's copy of a role-lane notice
  (LEGION-108). Before, only Dispatch and webhook delivery-id keys earned a MsgId. The same rule,
  `dedupeKeyNamesItsEvent`, now decides what a core-NATS host drops.
- The Dispatch dashboard no longer offers a same-mode **Retry** for a targeted message or comment
  mention its session answered with an error (a BTW whose side turn failed, a frame its host
  refused). The stream already stored that attempt's frame under the Retry's key, so a session the
  listener pushes to from the stream was never handed the Retry, and the attempt then read
  "Delivered by an earlier attempt". The card now says the session answered with an error and
  points at its mode-change actions; the mention list points at a new comment. Sending that Retry
  under a new key is LEGION-431. On a closed issue the mention list, like the card, no longer
  promises "Retry won't deliver it twice" beside a failure it offers no Retry for.
- `POST /v1/messages/publish` refuses a `dedupe_key` on a `source: "dispatch"` envelope with a 400
  naming `dedupe_key`. Every host drops a repeat of a Dispatch key, and Dispatch's outbox numbers
  its keys in sequence (`dispatch-<event id>`), so any holder of the listener bearer could publish
  `dispatch-<next id>` on a topic someone follows and make that host drop the real event when it
  arrived. Dispatch never set one there: its outbox publishes to the bus directly, and its sends
  go through `/v1/messages/send`, whose key the listener makes.
- Dispatch's conversation view (`/agents/<id>/live`) sends as Send by default wherever the
  session advertises steer, and as Aside otherwise, and names the modes Send, Aside and BTW. A
  person's message that an Oh My Pi session took as its own user turn shows once, where the
  session took it, still naming its author, and only when the streamed text is the stored one;
  on a host without `pi.sendUserInput`, one the session took between a run's last queue or aside
  poll and its `agent_end` reaches the stream untagged and shows twice
  (`packages/pi-envoy/AGENTS.md`). The Agents page shows an attempt the session accepted as
  conversation, with no retry, even after a later `failed`; every other attempt keeps its states
  (LEGION-394). Every label the dashboard composes for a mode now uses the composer's names (Send,
  Aside, BTW), the card's mode-change buttons included ("Use BTW instead", "Use Send instead",
  which read "Send as BTW instead" and "Send normally instead"). A delivery error or broadcast
  exclusion reason Dispatch stored keeps its wire name (`does not advertise steer`), as agents and
  scripts read it (LEGION-394).
- `GET /api/v1/issues` pages with `limit` (1–250) and `offset` (0 or more; `offset` alone pages
  50), answering `{issues, total, limit, offset}` with `total` counting every issue the filters
  match; without either parameter it answers the whole listing as an array, as before. It used to
  ignore both, and `cursor`, answering 200 with every row (on production, `?project=LEGSMOKE` with
  `limit=5`, `offset=5`, `cursor=abc` or `limit=999` each answered the whole project), which a caller
  that asked for a page cannot tell from one (LEGION-406). A repeated, blank, non-integer or
  out-of-range `limit` or `offset` is `400 INVALID_QUERY` naming the parameter, and so is `cursor`,
  which the listing does not page with. The page is cut after every filter, `route_status`
  included. The listing's order now ends on the issue key. Status, rank and creation time can tie
  (every project's first issue has rank `U`, and nothing makes a rank unique), and Postgres
  returned such ties in an order of its own: in a project whose issues share one creation time and
  alternate between two ranks, the old order listed one rank as `S8-2 S8-6 S8-4 S8-8`. A walk of
  the pages is exact only while the listing does not change. The Stage 4b live proof no longer
  sends `limit=200` (its preflight) or `limit=250` (its daily-report check); each would now answer
  a page its `jq` cannot read.
- The CI summary loop publishes a `pr.<n>.checks` settlement for every commit of a pull request
  whose checks settle, not only its current head, carrying the commit's `sha` as before. A head
  pushed with GitHub's `skip-checks` trailer runs no CI, so the commit it replaced settles for it
  after it lands (LEGION-208). Consumers decide which commit a settlement stands for: the
  TypeScript daemon still takes only its head's, and the Go daemon takes a code head's for the
  handoff-only heads after it. The listener no longer records each pull request's head (the
  `head.*` records in the CI bucket); a record an earlier listener wrote is skipped until its TTL.
- Every write of a CI record stamps it `schema: 1`. A record without it was last written by a
  listener that settled only heads, and it settles only when its last event is between the
  debounce and the debounce plus five minutes ago, so a head that finished during the handover
  still settles and the backlog of commits that listener never settled does not: on 2026-09-28
  production held 1,442 such records (595 `pr.<n>.checks` subjects, up to 168 hours old), which
  the first start would otherwise have published at once. Those records stay unsettled, without
  `schema`, until the bucket's seven-day TTL expires them; the `/metrics` gauge
  `envoy_ci_legacy_records_held` carries how many the last tick held back, and the first tick that
  holds any logs the count, a floor, once at INFO. An observation that changes the record
  stamps it and the commit settles as usual; a stamped record settles however long it waited,
  across a restart too. A rolled-back listener ignores the field. A listener built with #1526 but
  without this change publishes the whole backlog, so every head-gated listener moves straight to
  a build carrying this change.
- `GET /api/v1/search` refuses a `q` over 1,000 UTF-16 units, counted after trimming, with the
  ordinary `400 CAP_EXCEEDED` and a sentence saying what to send instead (LEGION-386), and a
  `project` that is not a project key with `400 INVALID_PROJECT`, where a lowercased key used to
  answer an empty result. A mistyped key that still has a key's shape, such as `LEGOIN`, is still
  searched and still answers empty. An empty `project` still searches every project. Both ride
  in the URL, and the load balancer in front of production Dispatch answers a URL longer than it
  accepts with a bare `414` before the server sees the request. So deploying this names the rule
  only to a request that still fits; it cannot answer the 26,637-character search that prompted
  LEGION-386. Only an updated `@sjawhar/pi-legion-envoy` in a session's profile (or the OpenCode
  or Claude Code plugin built from the same executor) keeps that URL from being sent at all,
  because its `dispatch_search` refuses the same rules before any request.

- A Markdown document now nests at most 100 blocks, and a document tree with a node more than 1,000 levels below the document, a node attribute value nesting more than 100 arrays and objects, or a mark attribute value nesting more than 99, is outside the Proof schema (LEGION-465, LEGION-535). The bounds sit where every read serves the tree: past about 5,000 levels the document token is JSON that `encoding/json` will not write from Go 1.27 or read in any version, and `GET /blocks`, which hashes each block's subtree apart, does work growing with the square of the depth. A live tree past either tree bound is treated as any other tree outside the schema: settlement writes no version, its reads and edits answer `409 DOC_SCHEMA` naming the repair, the document websocket refuses it, and an upload of replacement markdown repairs it (LEGION-469). A textblock's inline markdown nests at most 100 marks inside one another - emphasis, strong, strikethrough, links, images and code - and deeper content is refused naming the line. An accepted suggestion whose own markdown nests within 100 blocks but lands deep enough that the document would nest past them is refused as `400 INVALID_OP` on `replace_with`, naming how many blocks the result nests (LEGION-465).

### Fixed

- An ask hit's search excerpt, in `GET /api/v1/search` and in the write-time suggestions, is cut
  from the text its search vector indexes - the question, the options' labels and descriptions,
  the answer's text and the options it chose - where it was cut from the question, the options'
  raw JSON and the answer's text, so the excerpt could show JSON keys and braces (INFRA-569).
- An ask answer that chose no option reads `"selected": []` from `GET /api/v1/asks/{id}`, the ask
  lists and `GET /api/v1/me/answers`, and an `ask.answered` event's `answer` and `previous_answer`
  read it from `GET /api/v1/issues/{key}/events`, `GET /api/v1/artifacts/{id}/events` and the
  `GET /api/v1/events` stream, where each read `null`, including the rows and `ask.answered`
  events already stored with `null`; a new answer stores `[]` (INFRA-569).

- A document now strikes tilde runs as a comment does. A run of `~` pairs only with a run of its
  own length, so `Plain ~a~~ text.` and `Plain ~~a~ text.` keep their tildes where a document
  written through the API struck `a`; a run directly after an escaped tilde may open or close, so
  `Plain ~\~~ end.` strikes the tilde where a document showed `~~~`; and `~` runs pair apart from
  `*` and `_` runs, in the browser editor's order, so `x* ~~a*b~~c*` is emphasis `b~~c` in both.
  Reading a paragraph of mixed `~` and `*` runs is now linear in its length: a mebibyte of
  `~~a ` then `a* ` took over a minute. So is a paragraph whose `*` runs the rule of three keeps
  apart: 437 KiB of `**a ` then `a*a` took 100 seconds and now takes under one. A rendering stored
  correctly keeps its bytes, an image's alt text included: the writer takes first a spelling that
  reads back under the browser editor's rules and under both sets of rules Dispatch stored
  markdown under, so the spelling it wrote before stays wherever the browser reads it back as
  written. One the browser misread, such as text `~~~x~~` stored as `\~~~x~~`, which the browser
  strikes, is written anew (`&#126;&#126;&#126;x&#126;&#126;`) when its document is next
  rendered, and no stored document is rewritten. An image inside an image's label now adds its own
  alt text to the outer alt text, as the browser editor reads it. A `*` or `_` run beside a tilde
  now opens or closes toward it as the browser editor reads it, so `*~~**b~~*` is emphasis around
  struck `**b`, and `Plain b*~* text.` is emphasis `~` (INFRA-534).

- A bare email address, or one written `<a@b.test>`, in Markdown written to a document through the
  API is now stored and shown as a link to its `mailto:` URL, `[a@b.test](mailto:a@b.test)`, as a
  comment and the browser editor show it (INFRA-530). Dispatch linked it to the bare address, which
  the editor's link opener resolved as a page path under the document's own URL. A document that
  already holds `[a@b.test](a@b.test)` keeps that link's bare target until it is edited.

- A broker outage no longer ends the sessions whose leases ran out during it (INFRA-487). A session
  renews its lease through a broker, so while every broker process was down, or none could reach
  the database, no lease could be renewed, and the first sweep after the outage ended every
  enrollment whose lease ran out meanwhile: each such pod lost its grants and stayed locked out
  until it was relaunched. Every serving broker now beats on its own row of a `broker_serving` table
  (migration 0019) every `BROKER_SWEEP_SECONDS`. A gap longer than three sweeps after the latest beat is
  time no broker was serving, and until a broker beats again a lease that was still running at the
  last beat stays live, so the sweep does not end it. The next beat gives that gap back to every
  such lease that was not revoked, capped at `BROKER_LEASE_SECONDS` from then, and logs
  `broker: gave back the time no broker was serving …`. A revoked enrollment, and one whose lease
  ran out while a broker was serving, stays ended, and a renewal with another pod's or service
  account's token is still refused `POD_IDENTITY_MISMATCH`. It does not cover a broker that runs but
  that pods cannot reach (ingress, DNS, a NetworkPolicy, a stale `AGENT_SECRETS_URL`): leases still
  end then. Grant lifetimes and pending requests still run on wall-clock time, the first deploy
  restores nothing, and a pod whose renewer already exited 76 stays without one until it is
  relaunched. `config.Load` now refuses to start unless `BROKER_LEASE_SECONDS` is more than four and
  a half times `BROKER_SWEEP_SECONDS` (`2*BROKER_LEASE_SECONDS <= 9*BROKER_SWEEP_SECONDS` is
  refused), so a live session's lease cannot run out inside a gap the broker still counts as
  serving. The defaults, 900 and 5, pass. A token login (`POST /v1/launcher-credentials/token`)
  the broker could not complete (5xx, such as while the database is away) now spends neither its
  source address's nor its account's rate-limit bucket, so the Legion daemon, which retries such a
  login every few seconds through the outage, is not refused 429 on its first login after it.

- Every `envoy-infra` component refuses a number in an integer input that is not a whole number
  within the 64-bit integer range, at any depth (an object type's property, an array element, a
  map value), naming the component and the input's path and never quoting a secret value, before
  anything registers. Pulumi decoded such a number into the component's integer by conversion, so
  a typed SDK or Pulumi YAML program sending `replicas: 2.5` got 2 replicas with no error, and one
  past the range got whatever the conversion gave. The check reads the plugin's own schema, so an
  integer input added later is covered with no list to keep.

- Dispatch's event outbox publishes a backlog hundreds of times faster across a remote NATS hub.
  It sent each envelope and waited for NATS to answer before it sent the next, then wrote the
  answer to Postgres through the pool the API's requests queue for, so every destination of every
  event cost a network round trip and a write: at a 150 ms round trip it published 1.7 to 1.9
  events a second, and under load in production it fell up to about 29 minutes behind the events
  being created. It now sends a whole claimed batch (100 events) before it waits for any answer
  (`bus.Client.Window`), in id order, so NATS still takes each issue's events in order, and
  writes the batch's outcome in one statement on a database pool of its own (`store.Pool.Outbox`):
  540 events a second at a 150 ms round trip and 1,100 at 50 ms, with the API's pool busy
  (`TestOutboxThroughputUnderRemoteLatency`). An event is still marked published only once NATS
  accepted every destination of it. A publish NATS cannot take because the connection is down no
  longer backs its event off behind the events after it: the batch stops and waits for the next
  scan, so a backlog held up by an outage goes out in id order once NATS is back, where before the
  first events of an issue reached the Legion daemon after later ones, which it drops as stale.
  The shared database pool every request reads through holds 64 connections, from 16: under heavy
  agent load the API held all sixteen and every other caller waited behind them.

- Dispatch's server now reads a `|` after an even run of backslashes in a table row as the end of a
  cell, as the browser editor reads it: `| a \\| b |` is the two cells `a \` and `b` on both, where
  the server read one cell `a \| b`, so a document no longer showed one table in the live editor
  and the command line and another in the past-version view. A `|` after an odd run (`\|`) is
  still the cell's text. Code in a cell cannot hold a `|` right after an odd run of backslashes,
  since no markdown of a cell writes it: an API replace, delete by quote or accepted suggestion
  that adds such code is refused, naming the table, the cell and the header above it, also beside
  such code a browser edit left, and a browser edit that holds one is stored with that `|` written
  as text after the code (INFRA-172).

- A Dispatch page in Firefox now shows what changed while its live event stream was reconnecting,
  such as a session's answer to a model switch, without waiting for the stream's heartbeat.
  Firefox opened the stream only once its first byte arrived, and a stream with no backlog and no
  new event sent none until the first heartbeat, fifteen seconds later: until then the page stayed
  "Reconnecting…" and skipped the refresh a reconnect owes every view. `GET /api/v1/events` now
  sends an SSE comment, `: open`, with its headers, so the stream opens at once in every browser.

- A person who edits a document in the browser while an agent's write to it is published is now
  credited for the edit. ygo runs each change's observers after it unlocks the document, so the two
  changes' observers could run in either order, and when the agent's ran first it counted the
  browser's change as part of its own, which is credited to no one because the agent's write was
  credited when it committed; the browser's own observer then found nothing new and credited no one
  either. The edit was stored, sometimes inside the agent's version, but its author was on no
  version and owed none. The room's observer now takes the room's changes one at a time in the order
  they committed, from an update log ygo fills under the document's lock, so each change is
  measured against the room the change before it left, whichever observer gets there first; a
  deleted paragraph is credited too, and a heading id the editor derives still credits no one. The
  ygo pin moves from `v1.51.3-sami.3` to `v1.51.3-sami.4`, which adds that log
  (reearth/ygo#332).

- A browser that opens a document while Dispatch closes that document's room now keeps getting the
  document's edits. Dispatch closes a room when an issue closes, when it evicts a stale or failed
  room, and at shutdown, and each close takes ygo a moment: it closes the browsers it found, waits
  for them and makes the room's writes durable before it removes the room. A browser that connected
  in that moment joined the room all the same, and ygo closed only the browsers it had found, so
  that one stayed on a room the server no longer had: no later edit by anyone reached it, and
  nobody saw its own edits, until it reconnected for some other reason. The ygo pin moves from
  `v1.51.3-sami.1` to `v1.51.3-sami.3`, where a forced close also closes such a browser before it
  returns, so the dashboard's usual reconnect takes it to the live room (reearth/ygo#331,
  INFRA-403).

- An agent woken by a Slack message now reads the words the message carries, wherever it carries
  them. A Slack delivery's `text` is the message's own text and then every display string its
  blocks and then its attachments hold — the strings under `pretext`, `author_name`,
  `author_subname`, `title`, `text`, `alt_text`, `value` and `footer`, at any depth,
  each object read with those names in that rank and then its remaining keys alphabetically, so a
  section's own text precedes its fields and a button's label its hidden value — joined by line
  feeds, and the summary shows the first line. Blocks come before attachments because Slack
  renders a block as the message body and an attachment after it. An attachment's `fallback` is
  read last of all, after everything else that attachment carries, and only when nothing in that
  attachment said it already: it is the plain-text copy Slack shows where the attachment itself
  cannot be shown, so a coloured alert whose `fallback` repeats its own section delivers that line
  once, and one whose `fallback` says more delivers the attachment's body first. An app that
  posts a formatted message leaves `text` empty and puts the words in `blocks` or `attachments`,
  and such a wake used to carry a summary ending in `: ` and a payload with no words in it at all;
  an image-only post now carries the `alt_text` Slack renders in the image's place. A string an
  object has already shown under another of its own names is not shown twice; two objects saying
  the same thing are two
  things a reader sees, so a message whose fields read `Passed 3`, `Failed 0`, `Skipped 0` keeps
  both counts of `0` and two attachments both reading `ok` keep both. A string equal to the
  message's own text, or to one of its lines, is dropped wherever it sits,
  and a `rich_text` block that says nothing that text does not say
  already is left unread — that tree is the Slack client's structured copy of a person's words,
  and it is the one block kind the walk knows by name, which is why a message composed in a client
  reads as it did, the `&`, `<` and `>` its author typed included. An app's own rich text, which
  Slack tells app authors to post beside a short `text` field, says more than that field and is
  read like any other block, as is a `rich_text` block nested in one already read. The delivered
  text is still cut at 2,048 runes and marked `body_truncated`, and a deleted message still
  delivers nothing (INFRA-180).

- A Dispatch reference read no longer fails while the database holds an edge a newer build wrote.
  `GET /api/v1/references` (`?to=` and `?from=`) and `GET /api/v1/artifacts/{id}/references`
  answered `500` on a row whose other end is a node kind the running build does not know, which a
  person saw as "Referenced by unavailable." and an agent's `dispatch read` as one `unavailable`
  row; the build now leaves that edge out and lists every edge it knows. The `referenced_by_count`
  of an issue and of every ask leaves it out too, so a count is the number of rows its panel lists,
  and a document's `outgoing` no longer lists a target of a kind no client reads. A build that adds
  a node kind to the graph can therefore roll out while the older build still serves reads.
- The architecture import no longer fails on a branch head whose commit GitHub answers with more
  than the 1 MiB response cap, as a commit touching many files did. `githubapp.Client.Ref` asks
  `GET /repos/{owner}/{repo}/commits/{branch}` for the SHA alone
  (`Accept: application/vnd.github.sha`) and refuses an answer that is not one 40-hex SHA, which a
  directory read at it would have imported as an empty model (INFRA-390).
- `GET /v1/registry/<session_id>` is the listener's authoritative answer about one session, so a
  session that is working is no longer read as gone while a listener's session cache refills. The
  read goes through that cache to the session bucket: a 200 carries the same body a row of `GET
  /v1/sessions` carries (`session_id`, `machine_id`, `host`, `dir`, `port`, `title`,
  `capabilities`, `self_subscribed`, `topics`, `roles`, `updated_at`, `last_seen`), a 413 naming
  its size or a 400 naming the key refuses an id NATS will not take as a KV key, `404
  {"error":"no live session <id>"}` is only the bucket's word that it holds no key for that id,
  and a read that did not answer is `503 {"error":"envoy: the message bus did not answer:
  <reason>"}` with `Retry-After: 1`. Dispatch asks about the one session it holds the id of
  instead of fetching every live session and scanning the answer, so three decisions no longer
  turn on one listener's cache: a comment mention's `session:` target, a broadcast's recipients
  (each selected recipient read by its own id, eight reads in flight under one five-second
  budget, and a read that did not answer refuses the whole send `503 ENVOY_UNAVAILABLE` with
  nothing written), and whether an issue's claim holder is still working (the claim is free only
  on the bucket's 404; anything else is a 503, never a live session's claim taken without
  `{"force": true}`).
- A listener whose durable consumer was deleted and created again under it, so that the durable
  delivers to some other subscription, reports itself unhealthy and binds the durable again
  instead of receiving nothing while every check passed. `GET /healthz` answers `503`
  `unhealthy` with `durable consumer unavailable: not bound to this listener's subscription`,
  and the self-health monitor treats an unbound durable as terminal: it confirms the reading
  with a second one, rebinds, and reports success only once the server itself reports this
  listener bound (INFRA-182).
- No NATS server URL the bus logs carries its userinfo: `envoy nats reconnected` and the refusal
  of a NATS server that is not this machine's wrote the configured URL whole, and nats.go
  authenticates with a URL's userinfo, a user name alone as a token and `user:password` as a
  password, so `NATS_URLS=nats://user:password@host:4222` put the password in the process's log.
  Each URL is now written without its userinfo, as `envoy nats reconnect attempt failed` writes
  its servers (INFRA-182).
- `agent-secrets secret` writes on a dev box under the person's own Google sign-in, with no setup
  but `gcloud auth login`, and never tells a person to run `gcloud auth application-default login`,
  which replaces a machine's own credentials for every program that uses them. A write under
  Google Cloud Application Default Credentials that are a machine's own identity (a dev box's
  workload identity federation credential, a service account key, the metadata server's identity)
  runs under the person's gcloud login where it was refused, and so does every form on a machine
  with no Application Default Credentials: the CLI asks the first `gcloud` on `PATH` for that login
  (`gcloud auth list`, then `gcloud auth print-access-token`), reads none of gcloud's files, and
  leaves the machine's credentials as they are. A person's login is an account gcloud names by a
  plain email address, never a service account or a workload or workforce identity federation
  login (`principal://…`); where gcloud holds several, the command uses the one
  `CLOUDSDK_CORE_ACCOUNT` names, and a command it prints for running again (`pipe the value in: …
  < FILE`) starts with that `CLOUDSDK_CORE_ACCOUNT=…`. gcloud runs with its file logging off and
  prints the token in its private format, so the token reaches none of gcloud's log files, and
  with impersonation, an access token and a credential file override cleared, and a write whose
  token Google answers as another account's is refused. A write with no person's login, or several
  and none named, is refused before the value is read, naming `gcloud auth login` or each login,
  and a refusal names the credentials it found by their type, never by a file. Reads under a
  machine's own credentials, and every form under a person's own Application Default Credentials,
  are unchanged. `GOOGLE_APPLICATION_CREDENTIALS` stays the person's own choice: a file it names
  that cannot be loaded is refused naming that variable (the Google library's error, quoted, can
  name the file), a machine's credentials it names are refused for a write rather than replaced,
  and a command printed for running again starts with `GOOGLE_APPLICATION_CREDENTIALS=…`, naming
  the file. The `--profile` refusal under the GCP store names the Google sign-in it uses instead.

- With two `envoy-dispatch` processes on one database, a document whose room one process held
  behind the durable head no longer answers every write with `503 DOC_SERVICE_UNAVAILABLE` for a
  while, and a version named there no longer records text missing the other process's edit
  (INFRA-218). A settlement the durable gate refused from such a room was retried on the same
  room, which can never pass the gate, so its third refusal failed the room
  (`document settlement failed 3 times`) and every transaction on the document was refused until
  the eviction ran. The refusal now closes the room and settles it again from the reloaded one. A
  named or snapshot version taken from a room behind the head rendered that room's text while
  recording the stored head, so no later settlement versioned the other process's words, and it
  listed the room's unstored browser edits, whose authors were listed again once the edit was
  resent; it now renders the stored document and lists none of them, and neither does a version
  that a write of its own renders over the stored document (an edit with a summary, an accepted
  suggestion, an anchor's snapshot). The settlement's close and the stale-room evictor's both
  reserve the room they checked, so a write that would open between the check and the close
  answers 503, and the caller's retry lands on the reloaded room, instead of being forgotten with
  the closed one; and neither closes a room that replaced the one it checked, including one ygo's
  idle sweep retired and a load replaced before the close reached it. A write that opens while a
  settlement is armed, fired or running takes the settlement over and arms it when it ends, so a
  write that changed nothing no longer leaves the document's settlement to the one-minute
  resumption.

- A person who opens a Dispatch document while the service is force-closing that document's room
  keeps the credit for what they type (INFRA-352). The service forces such a close when the room
  fails, when another process's write leaves it behind the stored document, and when a test or the
  browser-test harness asks, and it threw the room's in-memory record away once the close
  returned. The browser registers before its room is loaded, the room that replaces the closed one
  loads, and an edit typed in that room is credited, all while that record is still the one the
  service holds, so the throw-away took the person's credit with it: the next version left them
  off its author list. It also lost the issue's closed flag, credited the document's existing
  decision blocks to whoever edited next, and left the settlement the new room had armed to run a
  settle delay late. The close now keeps the record while anything holds it and forgets it when
  nothing does, as every other end of a room does. Only a failed room's record still goes, since
  it carries the failure, and the browsers that joined while it was evicted are handed to the
  record registered in its place. A room that loads onto a kept record takes the latest edit
  source the stored document names, none included, unless the record recorded a newer one, so
  another process's edit that several browsers could have made is not credited to whoever edited
  here before; the closed room's settlement failures end with its generation rather than once the
  close returns, so they never wipe the failures of the room that replaced it, and an issue's
  close ends them the same way; the close of a room behind the stored document carries the
  service's own mark, so a closed issue no longer refuses it and such a room is closed rather than
  left resident with text the store no longer holds until its idle minute runs out; and the
  browser-test harness's quiesce drops the edit source of
  the settlement it cancels, and once its closes have ended drops any source a reconnect or an
  edit recorded meanwhile, so the record of a closed document it truncates away is not kept for
  ever.

- A burst of device logins no longer stalls the secrets broker's database pool. Each
  `POST /v1/oauth/device_authorization` held its provider transaction's pooled connection while
  it recorded its request object's `jti` on a second one, so as many concurrent logins as the
  pool had connections (the pgx default, the greater of 4 and the CPU count) waited on each other
  until the 45 s request deadline, and every database route, `/healthz` among them, waited
  behind them; the route needs no credential, and a `login_hint` per request slips its person
  bucket. The `jti` is now written in the provider's transaction, so a device login holds one
  connection, and a refused login leaves no `jti` behind.

- A publish to an ad-hoc topic (`envoy_publish` to a `notifications.` topic no stream subject
  captures, such as `notifications.team.deploy-squad`) reaches its subscribers again, and is
  answered at once. The listener sends such a topic over core NATS, confirmed as a role lane is
  (`bus.usesCoreTransport`), where it went to JetStream, which never answers a subject no stream
  captures while a core subscriber holds it: the publish waited out its five-second
  acknowledgement deadline and answered 500 `context deadline exceeded`. The listener's NATS grant
  (`ListenerPermissions`, and so `HostPermissions`) publishes on `notifications.>`, where it
  listed only the stream's subjects, the role lanes and the exception lanes, so a hub that renders
  the grant lets the listener make that publish; its rendered authorization changes, which
  replaces a GCP hub's VM on its next update. `POST /v1/messages/publish` now answers
  `400 {"error":"topic must begin with notifications.","expected":["topic"]}` for a topic outside
  `notifications.` and publishes nothing, which closes the raw-subject route: before it, a caller
  holding the listener's bearer could publish to any subject the listener's own NATS user may
  publish — measured on a production-shaped rig at main, a publish to
  `$KV.envoy_roles.example-role` answered 200 and the role-claim bucket went from zero messages to
  one — and could reach `$JS.API.*` the same way. It closes that route alone: the listener's own
  role-claim endpoint still serves any holder of the token, by design, and is not changed here.

- The listener's `GET /v1/interests/<session_id>` answers 404 only for a session the registry
  holds no record of (`nats.ErrKeyNotFound`), and 503 for a read NATS did not serve (a dropped
  connection, a timeout, a record that does not decode). It used to answer 404 for every failure,
  so a caller took a NATS failure for a session subscribed to nothing: the `dispatch` command
  printed "not subscribed", and Dispatch's subscriber removal could complete while the interest
  was still stored.

- A bare URL next to punctuation in a Dispatch document links as the browser editor links it, so
  reading a table row with `dispatch doc-read` and writing it back with `dispatch doc-edit` no
  longer grows the link. `"HTTP 401 (https://api.github.com/graphql)" while` links only
  `https://api.github.com/graphql`, where the server linked `https://api.github.com/graphql)"` and
  stored it as `[https://api.github.com/graphql)"](<https://api.github.com/graphql)">)`. Where the
  server reads a URL - after whitespace, `*`, `_`, `~`, `(` or a line's start, and after quotes and
  other punctuation that follow one of those - it reads a www or http(s) URL as GFM's extended
  autolink does, as the editor's parser reads one: trailing punctuation before whitespace, `<` or
  the line's end (`"`, `'`, `;`, `]`, `_`, and a `)` that closes no `(` in the URL among it) is left
  out, a backslash, an asterisk or a letter outside ASCII is part of the URL, the scheme reads in
  any case, a domain with an underscore in either of its last two parts is text, and `ftp://` is
  text. A URL the editor links after a letter and punctuation (`x;https://…`), after an escape or a
  character reference, or at a line or cell opening with punctuation is still text on the server.
  A bare URL the editor's rules read whole, its scheme in any case and a `www.` one included, is
  written as its own bytes, or, where the text around it would change what is linked, as `<url>`
  (a www link as `[www…](http://www…)`).

- A write that rewrites a block's text no longer stores two U+FFFD in place of an emoji: an edit
  (`dispatch doc-edit`, `POST /api/v1/artifacts/{id}/edits`), an uploaded new version of a live
  document, an accepted or rejected suggestion and an ask edit. The new text is written as one span
  of the old text replaced, and two emoji that share a high surrogate (📊 and 📋) or a low one (😊
  and U+1FA0A) agree on one UTF-16 unit of their pair, so a span placed by comparing units began or
  ended inside the pair and each half was stored as U+FFFD. Rows a table edit moves are rewritten
  this way too, so an emoji in a row the edit never named could break. The span is now of whole
  characters. A document already damaged stays damaged.

- A Mermaid sequence, class, ER or xychart diagram in a Dispatch document renders. The editor's
  Mermaid preview ran its flowchart checks on every diagram, so every type other than a flowchart
  or state diagram was refused with "Unable to render Mermaid diagram: Invalid mermaid header";
  those checks now run on flowcharts and state diagrams only, which still refuse a dangling edge.

- A closed issue can give up an external link (a write whose links are a subset of what it already
  holds) so the link can move to the issue that replaced it; adding a new link to a closed issue
  still refuses with `ISSUE_CLOSED`, as every other closed-issue content write does.

- A bus client whose NATS connection closes while a publish is in flight now replaces it once,
  and `Close` and `Drain` close every connection it dialled (LEGION-765). The recovery the close
  starts and the publish each dialled a replacement, and both were installed in turn, so the
  first was left connected and was never closed. Once its server went away it reconnected forever,
  once a second, in the listener, Dispatch or any other process holding a `bus.Client`. Now the
  second to finish closes its own connection, logs `envoy nats discarded a second replacement
  connection`, and serves with the first. Its close logs no `envoy nats disconnected` and starts
  no recovery. No message is lost or sent twice: the discarded connection never carried one.

- Dispatch's job intake now receives job events (LEGION-690). It shared Dispatch's publisher
  client with the delivery intake, and a `bus.Client` held one JetStream subscription, so
  whichever intake bound second unsubscribed the other, whose durable then sat unbound with its
  messages pending. Each intake now connects on its own, and a `bus.Client` refuses a second
  subscription on another subject (`bus.ErrSubscriptionHeld`) rather than replacing the first.
  With the GitHub App configured, Dispatch holds four NATS connections where it held two, named
  `dispatch`, `dispatch-agent-stream`, `dispatch-intake` and `dispatch-job-intake` on the server,
  and exits at boot when it cannot open them, so a NATS user or account with a connection limit
  must allow four; without the App it holds two, as before. Each bus client's lifecycle log lines
  now carry its name. On its first start the job intake's durable is handed every job event it has
  not acknowledged, up to the stream's 72-hour retention, and reads at most eight runs' jobs from
  GitHub at once while it drains, before it settles at nothing pending.

- The `GET /ws/delivery` socket gives itself a read buffer, so a page that stops answering its
  pings loses the socket with no close frame (1006 to the page) under any gorilla/websocket
  (LEGION-690). Without one, a build linking a gorilla that keeps net/http's hijacked reader (every
  workspace build, through `packages/daemon`'s client-go) could close it with 1008, the code for a
  caller that no longer resolves, depending on a race. The published image, built in module mode
  with gorilla v1.5.3, never did. The page reconnects with backoff after either close.

- The delivery live hub arms its one timer for the moment the next view is due, rather than for a
  wait counted from the moment its worker read the clock (LEGION-690). A wait put the timer late by
  however long the worker took between that read and the arm, and a test that moved its clock
  forward in that gap saw no timer fire at all, so its Pipeline view never moved past midnight. A
  timer armed for a moment the clock has already passed fires at once.

- The delivery live hub no longer re-sends a run in which nothing changed (LEGION-690). Each item it
  sends is revised by a hash of its JSON, and several store reads answered rows tied on their order
  in whatever order Postgres's plan read them: a run's jobs (no order at all), the pull requests a
  run shipped and the waiting list (merged in one second), and runs started in one instant. The
  plan alternates between a sequential scan (the table's row order, where a reconcile pass's
  unchanged rewrite moves a row last) and an index scan, so a recompute answered a run's
  `failed_jobs`, `root_failing_job`, `production` or `prs` in another order and the hub sent the
  run again, whole: in production deltas of 54 to 61 runs, about 110 KB each, whose one change was
  the order of their `prs`. A run's jobs now come back by when each started, a job that never
  started last, then by name (`ListRunJobs`, `ListRunJobsForRuns`, sorted in Go per run: an
  `order by` sorts the whole 28-day read on disk, 82-93 ms against 43-52 ms for the unordered read
  and 3 ms for the sort), the order `GET /api/v1/delivery/runs/{id}` already listed them in. Pull
  requests merged together come back by id (`owner/repo#N` in byte order, the page's own order) and
  runs started together by run id. A run's root failing job and its production job, among jobs
  finishing together, are the first by name, and a run whose production jobs all failed falls back
  to the first by start rather than the first listed. The Pipeline page's failing jobs name, as a
  job's last failing run among runs that completed together, the one with the higher run id rather
  than whichever was listed last.

- Two production deploys whose jobs complete in the same second are ordered by run id, and the
  lower run id ships a pull request both could, so the live Delivery hub's sliding window no longer
  swaps a pull request's `deploy_run` and a run's `prs` with no stored row changed (LEGION-690).

- The approval carry holds the version a human approved to the document's asks rows as it holds
  the version his answer writes (LEGION-752). A block of the new version counts as unchanged since
  his approval only while the approved version holds a block under the same id whose stored
  attributes and, for a decided row, wording are what settlement's own writer leaves from that
  block's row - the whole server-owned set, not `state`, `answered_by` and `answered_at` alone -
  so an approved version stored inside the settle window, showing a selection, an answer, a copy's
  provenance or a question other than its row's on a decided block, no longer compares as
  unchanged against the row-true version his later answer writes: that block is a change, which
  must be his own answer since his approval, and his answer to another block carries nothing
  (`stale`, as before the carry existed). The same sequence over an approved version whose block
  is as its row has it carries, as before. The approved version's blocks are read as the new
  version's are (`askNodes`); a stored version's read-back repairs a repeated block id before the
  carry reads it, so a stored version that held an id twice shows a block the new version never
  had and carried nothing before this change either.

- A picture written into a spec or document as `![name](dispatch://KEY/artifact/<slug>@vN)`, the
  line a pasted picture is written as, now shows in the document editor and in a past version,
  drawn from the version's bytes route; before, the editor's `<img>` carried the `dispatch://`
  address as its source, which no browser fetches, so the line showed nothing. A version the
  browser cannot draw (a markdown document, another file, a missing version), or a source that
  pins no version, shows a link to what it names instead, its text the reference's short form
  (`CORE-1 notes-md`). A picture from another website is drawn as before (LEGION-695).

- A listener's shutdown drain no longer waits out 5 s of its 10 s budget when its NATS connection
  closes while nats.go drains it. nats.go v1.50.0 marks a connection closed during its drain as
  draining again and waits 5 s for a reply on the socket it closed, and the drain read the
  connection's state, so it could miss the close; it now waits for the connection's closed event.
  The listener's own reconnect callback was one such close: nats.go can run the callback for a
  reconnect that came before the drain once the drain is under way, and it closed the connection
  there, cutting off the deliveries still in their handlers. A reconnect callback now closes the
  connection only for a reconnect that came after the drain began.
- A person's new comment on a document reaches the agents that wrote it, each once, on its own
  `notifications.agent.<session_id>` topic, whatever the issue's route: a comment on a passage of
  an issue's document or a project document, a comment on a whole project document, a suggestion,
  and an edit of any of these. The writers are the session that created the document and every
  session one of its versions credits, which includes an agent whose anchored comment credited it
  on a new version. Before, a new comment found no author to route to, so the agent heard of it
  only by subscribing to the issue's or document's own topic (LEGION-681). A document only people
  wrote sends nothing new, since a person has no topic. A reply still reaches its thread's root
  author and its parent's author alone, an ask reply its followers, and a comment on an issue's
  conversation, which names no document, no author. An agent's comment still notifies nobody, so a
  session commenting on a document it wrote is not told of its own comment. A writer the comment
  @-mentions hears it once, from the mention: a new comment's `suppressed_authors` now names every
  session a mention resolved to.
- `scripts/dev-postgres.sh` reuses its `dispatch-pg` container rather than failing on the name: it
  starts a stopped one and serves a running one, when the container runs the pinned pgvector
  image. It refuses one on any other image, such as a `postgres:16` an older version of the script
  created, whose missing `vector` extension failed Dispatch's migrations at `0072`. The refusal
  names both images and prints the commands that move the container's data volume onto the pinned
  image, read from `docker inspect` and quoted so a bind mount's path with a space pastes whole;
  the script itself stops, removes and recreates nothing. `scripts/dev-broker.sh` boots Postgres
  through it, so it refuses that container too, before it creates a database.
- A document whose search vector would pass Postgres's limit on one vector (1,048,575 bytes of lexemes and positions) can be versioned. Text of words no two alike passes it well inside a document's 1 MiB, at about 700 KB of `w000001 w000002 …` or 475 KB of UUIDs, and every write of such text failed with `string is too long for tsvector`: the document's version, so every settlement of it, its upload, an issue titled with it and that title's duplicate check, and an ask block holding it. Migrations `0087`–`0088` build every search vector with `search_vector`, which indexes the whole text where its vector fits, exactly as before, and otherwise the longest of its first half, quarter, eighth, … that fits, cut between two words, so keyword search finds such a text by the whole words that open it and by no part of a word the cut would split. A body with no whitespace before its cut contributes none of its own words; an issue's key is always indexed whole (LEGION-505).
- An issue title is at most 1,000 characters (`contracts.IssueTitleMax`, UTF-16 units after the trim), on creation and on a retitle; a longer one is `400 CAP_EXCEEDED`, named as the comment, message and ask caps name theirs, before the duplicate check reads the project's titles. Titles had no cap, so one stored title could be as long as a request allowed: every later creation in its project read it in the duplicate check, and the `409 POSSIBLE_DUPLICATE` message quoted it whole. Beside 2,000 production titles, one stored 800 KB title raised the check from about 0.1 s to 0.56 s. A new title of 20,000 distinct words (10,000 did not) beside an issue whose title shared one of them overflowed Postgres's default 2 MB stack in the duplicate check's snippet (`stack depth limit exceeded`, `500`); a title at the cap holds at most 501 lexemes. Production's longest title is 326 characters. The dashboard's create dialog and issue-header title field stop at the cap, and a retitle Dispatch refuses shows Dispatch's reason in the header instead of the generic failure line (LEGION-505).
- The duplicate-title check no longer parses every title in the project on each creation. It read each stored title's lexemes by building them from the title as it ran, several times over for a title that shared a word with the new one, so its cost grew with every title in the project: beside 2,000 titles at the 1,000-character cap, an ordinary creation's check took 2.1–2.5 s and one at the cap 7.6 s. Migrations `0088`–`0089` store each title's own lexemes in `issues.title_lexemes`, which the issues trigger fills on every insert and retitle and `0089` fills for every issue stored before, under a table lock that no reparent deadlocks against; the check reads them, so a creation parses one title. Beside those 2,000 titles the check takes 56–95 ms, and beside 2,000 production titles 15–19 ms where it took 120–154 ms (Postgres 16, load 60–80). It answers the same candidates, in the same order. A candidate's snippet marks the words its title shares with the new title: with a parent, a word that is both the parent's and a part of a hyphenated word of the new title is no longer marked, where the snippet marked it beside parent `Legion` in `launch window legion` for the new title `legion-resolve launch window` (LEGION-505).
- The delivery timeline and measures no longer log `a successfully concluded run has no job
  matching the configured production_job_name` for a deploy run that skipped its production job.
  GitHub lists a skipped call of a reusable workflow under the calling job's name alone, so a
  dev-only run lists `production-apply` where a deploy lists `production-apply / production-apply`.
  The skip is taken as the reason only while another run the same read judges holds a job by the
  configured name, so a typo whose first part names a skipped job (`staging-apply /
  production-apply`) still warns; a window with no production deploy in it warns for each of its
  successful runs. Such a run still counts as no deploy. Every warning is now logged once per run
  and configured job name for the life of the process rather than on every read: one dev-only run
  had been logging the line twice every three minutes (LEGION-567).
- A document uploaded or seeded with another document's ask blocks, under their block ids and with
  the same question, options, `multiple` and urgency, on the same issue (or the same project for a
  project document) opens no ask for them: each copied block shows the state and answer of the ask
  it was copied from, read-only, and names it and its document in the server-owned `copied_from`
  and `copied_from_document`, where settlement had opened a new ask in a person's Inbox for every
  copied block, answered and resolved ones included. Of several matching asks the earliest asked
  is the source, whichever wording it matched on. A block matching only a source's earlier wording
  is a copy of it while the source is open, or when the source's answer or resolution was given
  while it asked that wording, ordered by the events' commit order; one answered or resolved after
  the rewording opens its own ask, so no copy shows a decision on a question it does not ask.
  Answering or resolving the source reaches its copies a settlement later. Each block settlement
  takes for a copy, and each change of the state a copy shows, is logged once (`dispatch: settled
  an ask block as a copy of another document's ask`, naming the copy, its block, the source ask,
  its document and its state, never the question), since such a block opens no ask and reaches no
  one's Inbox.
  `dispatch request-approval`'s refusal and the dashboard's decision card name the source, its
  document and its state from those attributes. A copy whose text is changed, or an id no ask of
  the owner indexes, still opens one, and so do a source's copies once its block
  leaves its document: settlement of the source retracts its ask and settles every copy, the first
  opens one ask, credited to whoever wrote the block into it, and the others name that ask, on a
  project's documents as on an issue's. Migration `0091_artifacts_project_documents` adds the
  partial index a project document's lookups read. An answered or resolved ask's wording is fixed:
  a document edit or an upload that changes its block's question, options, `multiple` or urgency
  no longer rewords the ask and leaves its answer beside a question nobody answered; settlement
  writes the decided wording back into the block and logs it, as `PATCH /api/v1/asks/{id}` already
  refused such an edit (`ASK_NOT_OPEN`). Each ask an earlier release reworded after its decision
  gets back the wording that decision was given on, in code and not by a migration: every boot
  restores them all in one statement and logs each ask, its document and its question, and each
  document's settlement restores its own before it writes the decided wording back into the block,
  which also undoes a rewording an earlier release's task writes during a rolling deploy. The boot
  settles each restored ask's own document, whose block still asks the rewording until that
  settlement writes the decided wording back. Either restoration settles the restored asks' copies
  again: a copy that settled while its source's row held the rewording showed the decision on the
  reworded question, and now opens its own ask (LEGION-651).
- A document opens in the editor however many documents the process has touched: the 1,000-room
  cap counts ygo's live rooms, and a document's in-memory state is released once its room goes and
  nothing still holds it. Before, every document opened since a restart kept its state and counted
  against the cap, so editors were refused with 503 after about 1,000 (LEGION-513).
- A document's pending authors now survive room release, process restart and overlapping Dispatch
  tasks in `doc_pending_authors` (migration `0084`). A browser update is first an in-flight,
  room-local credit (F); its append moves an unconsumed credit to the durable record (R) under the
  document lock. A joined write records its authors in R in its content transaction. A version
  reads R under that lock and may capture F only from its own room; after its transaction commits,
  it deletes the R rows it listed and consumes the F credits it listed. The scoped rule means a
  task can list another task's durable R records but never that task's F, while the same task can
  consume F before its queued append can re-record an author. Each author is consequently pending
  in F or R, or listed on one committed version, rather than in more than one of them. A settlement
  that writes no version leaves R intact, and an upload that writes a replacement clears all R and
  only the F credits present at its last room read. The document room can therefore go idle without
  retaining author state or losing the authors a later version, ask or event must name
  (LEGION-513).
- `GET /api/v1/asks/open` and `GET /api/v1/me/answers` give an issue ask's `ref` as its item
  route, `/issues/<KEY>/asks/<id>`, where they gave `/issues/<KEY>?ask=<id>`, which the bare
  issue page does not read, so following it landed on the issue and not the ask. A document ask's
  `ref` is unchanged (LEGION-622).
- The secrets broker no longer logs `agent secret policy load failed; previous policy kept` for a
  periodic reread of the namespace that its own shutdown cut short: `policy.NewCurrent` logs a
  failed reload only while its context is live, so the deployment's alarm on that line no longer
  counts a shutdown as a failed load. A reload that fails while the broker runs logs exactly as
  before.
- Agent bearer tokens can now list repository-to-project mappings and architecture sources, and set
  or remove repository mappings, architecture sources, and delivery settings. Settings writes record
  the bearer-supplied session actor, as other agent-authenticated writes do.
- Every read of an artifact's `project_key` tolerates a null: `scanArtifact` (every artifact read
  by id, ref key, owner or name, and both anchor locks), an ask's anchor artifact, a comment
  event's and an anchor refresh's payload, a suggestion's project, a document write's owner lock,
  and search's owner project (a nil dereference there, not a scan error) answer an empty project
  instead of `cannot scan NULL into *string`; the reference graph gives such an artifact, and an
  item on it, no address rather than `dispatch:///artifact/<slug>`. Landed on `main` ahead of
  migration `0071_agent_artifacts`, which gives an artifact a third owner with no project
  (LEGION-541), so a binary from before that migration, serving beside it during its rollout or
  after a revert, reads those rows.
- A published edit's check of whether a browser's concurrent change removed its text (`lost_ops`, and an accepted suggestion's `lost`) walked the room's live tree without its lock while the room's browsers wrote it, so it could read a keystroke halfway. Every read of a resident room outside a write now reads the room as of one moment under its lock: the replica the room's update observer keeps, brought up to date, or, while the observer or another read holds that replica, a copy as before. A read only tries the replica, so reads never queue behind each other or ahead of a keystroke's observer, but a keystroke can still wait for one read already walking it (about 100 ms on a 524 KiB document). A read through the replica takes about a third of a copy's time (86 ms rather than 283 ms for a 524 KiB document's tree). A tree a read returns shares nothing with the document it was read from, so editing it changes no later read (LEGION-499).
- A deploy with someone on the Dispatch dashboard no longer spends the whole shutdown waiting and
  then leaves the documents it owed unsettled (LEGION-501). An open event stream or agent
  conversation stream never went idle, so `http.Server.Shutdown` held for its full 5 s, and the
  document service then shut down on that expired deadline and logged `document settlement
  unconfirmed at shutdown`. Both streams now end at SIGTERM, which the dashboard reconnects from,
  so HTTP shutdown waits only for the requests in flight. Those requests drain first, with no
  deadline of their own; the document service starts once they are done, or 11 s after the signal
  with some still running, and gets 14 s of its own, so it finishes 5 s inside the 30 s stop timeout
  ECS and the compose file give Dispatch. Each loaded document drains, reads what it owes and
  settles in a worker of its own within 9 s of that budget, so a burst of writes still landing
  after the worker started is waited for too, not only the ones already queued, and a settlement
  still committing once that 9 s ends keeps the remaining 5 s to finish rather than ending in an
  unconfirmed error: a document whose update is slow to store, or whose editor
  keeps typing, leaves only its own settlement to the next process. A spec with its tab open is
  settled while its room is still loaded, and its editors are disconnected only after that, so the
  edit they made is versioned before the process exits instead of when someone next opens the spec;
  a settlement that has to write into the room (stamping a block id, restoring an ask block's state)
  is still left to the next process, now with a WARN rather than an ERROR. The database pool closes
  once every request has answered, with nothing but the runtime's kill bounding that wait while the
  database answers, so a write waiting on a lock commits and is answered if it finishes before the
  kill. A database that has stopped answering no longer holds the process until its runtime kills
  it: once the document service has finished and three health probes in a row have failed with no
  connection returned meanwhile, Dispatch exits without the connections waiting on it (`dispatch:
  exit with database connections still in use once the database stopped answering`). The compose
  file sets `stop_grace_period: 30s`.
- A document's stored update log kept every byte any write had inserted, and every cold load of it built all of it: Dispatch merged the stored updates whole, and a merge keeps the content of deleted items. Five hundred 2,000-character replies to one anchored comment, each projecting the thread's margin record again, left 257 MB stored under a 3 KB document, and a cold one-word edit of it then took 1,842 MiB, past the 1,024 MiB task. A load now applies the stored updates one at a time to a document that collects garbage and returns what it holds, so it costs the live document and one update; and compaction, which runs as a room closes, as the server shuts down and daily, folds the whole log into that state rather than keeping the newest 500 updates beside a merge of the rest. An update a load's document parks for a dependency the log lacks is merged back into that state. A log the fold cannot apply, or a state that does not read back as the document that made it, is not used: the load merges the stored updates whole as before, and compaction leaves them as stored. After compaction, `doc_updates` and any backup of it hold no text a write deleted, so text deleted before a version captured it is gone (Sami's decision, LEGION-496). `doc_updates` is snapshotted once, and the snapshot kept 90 days, right before the first deploy that carries this.
- A document edit that repairs an ask a browser left unreadable now reports it in
  `decision_blocks_added`, matching the open ask its next settlement creates. An edit keeps that
  count as count-only advice when the bounded issue-advice query fails (LEGION-470).
- A NUL character (U+0000) in caller text no longer answers `500 INTERNAL`: PostgreSQL's text and jsonb cannot hold one, so every Dispatch route that stored such text (issue titles, specs and labels, comments, messages, broadcasts, asks and their answers, document uploads, edits and versions, a bearer's `actor`, a session id in a path) failed at the database. The API's input layer refuses it once instead, before anything is written: `400 NUL_CHARACTER` naming where it stands and its position, as in `options[1].label holds a NUL character (U+0000) at character 2, which Dispatch cannot store`; a NUL in a name the caller wrote is shown as `\u0000`, so no refusal sends one back. It reads every string and member name of a JSON body, a multipart upload's fields and markdown file, the body a credential decision or machine-login lookup relays to the secrets broker, and every route's path and query parameters, so a read such as `GET /api/v1/issues?label=` refuses one too. The document websocket refuses, before the upgrade, a bearer whose `X-Dispatch-Actor` JSON spells one in its session id or origin: that actor became the author of the versions the connection's edits made, so every settlement of the document failed and the third failure in a row failed its room. The check covers every parameter, field and member a request carries, so these requests main accepted are now refused: a retype whose typed attribute holds a NUL, an insert whose markdown holds a raw NUL inside a typed block's quoted attribute (written as the escape `\x00`, the same value is still taken), a query parameter or multipart field no route reads holding a NUL or a byte that is not UTF-8, and a websocket `X-Dispatch-Actor` whose unread member spells a NUL (LEGION-507).
- A byte that is not UTF-8 in a path or query parameter (`%FF`), in a multipart upload's field or in a binary file part's `Content-Type`, which the upload stores as the artifact's type, no longer answers `500 INTERNAL`, and one in an uploaded markdown file no longer panics the server, which dropped the connection: PostgreSQL's text cannot hold such a byte, and the document writer refuses one by panicking. The input layer refuses it where it refuses a NUL, `400 INVALID_UTF8` naming the field, the byte and its position. A JSON body cannot carry one, since decoding writes each such byte as U+FFFD (LEGION-507).
- A NUL character a browser edit puts in a document no longer stops the document from settling. The Yjs update reaches the live document past every route's check, and the version settlement rendered from it, an ask block's question and an anchored comment's or ask's quote each failed at PostgreSQL, so settlement failed every time it ran; with the character in the document's text or under an anchor, every agent edit and named version of the document answered `500` too, until someone deleted it in the editor. Dispatch now writes the character as U+FFFD, which is what CommonMark reads it as, in the markdown it renders and serves and in the ask and anchor text it takes from the document, and matches a quote, a `# Title` quote and a `heading:` anchor against that text, so one copied from `GET /text` finds its target; a typed block's other attributes keep it, written quoted as the escape `\x00` that reads back as U+0000 (LEGION-507).
- A block id holding a NUL, which a hand-built client can set on a document block, no longer stops the document from settling. An ask row or anchor could not store it, so settlement failed every time it ran, a comment or ask anchored inside that block before settlement answered `500`, and while it was a typed block's id `GET /text` and `GET /blocks` served it and naming a version answered `500`. Settlement now mints a fresh id for it, as it mints one for a missing id; until then an anchor taken in the block is pinned to no block, and every read and named version writes the id with U+FFFD in place of the NUL. Every other id stays as the browser set it, such as `q:1` from a pasted `:::ask{#q:1}`, which the browser editor keeps and an ask row stores (LEGION-507).
- An artifact upload whose multipart body the parser cannot read, such as one whose part header holds a control character, is `400 ARTIFACT_INPUT` with the parser's reason. It used to be answered `413 CAP_EXCEEDED "artifact blob exceeds 25 MB"`, as was every multipart parse failure; only a body past the upload's size limit, or form fields too large to read, is still 413, and a file part the server cannot spool to its temporary directory is `500 INTERNAL`, logged, since the failure is the server's (LEGION-507).
- The architecture sync no longer answers `500` and records nothing when a component's front matter spells a NUL through a YAML escape, in a value (`title: "Ser\0ver"`) or an unknown key (`"\0": value`): the parser checked the file's bytes, which are ASCII there, and the projection or the failure-recording update then failed at PostgreSQL with no `last_error` and no `architecture.sync_failed` event. The parser checks each decoded front-matter value and names its field; before `last_error` and the event payload are stored, every failure message passes through `text.StorableReplacement`, which writes a NUL or a byte that is not UTF-8 as U+FFFD. Any projection PostgreSQL refuses is recorded on the row like a model problem, so `POST /api/v1/projects/{key}/architecture-source/sync` answers 200 with `last_error` and the previous projection stays (LEGION-527).
- A Dispatch page whose HTML and assets came from different builds stayed blank: during a rolling deploy the load balancer sent a browser's HTML to one task and its hashed entry chunk to the other, which never had it and answered 404, and nothing reloaded the page, since the lazy-chunk recovery runs only once the entry has. The page now reloads itself once when one of its own build's scripts (same origin, under `/assets/`) fails to load, from the same once-per-session budget as the lazy-chunk recovery, never while offline or while the page is being left (from `beforeunload` or `pagehide` until `pageshow`); another origin's script, such as an extension's, never triggers it. Every page the server serves (`index.html` and the shell for a browser route) now carries `Cache-Control: no-cache` and an `ETag` of its bytes in place of `Last-Modified`: a browser could run a heuristically fresh page from an older build after any deploy, and a revalidation by file time could keep another build's page, since a task holding an older file (a rollback, or the other side of the overlap) answered it 304. Every file under `/assets` carries `public, max-age=31536000, immutable`, and `GET /index.html` answers the page rather than redirecting to `/` (LEGION-521).
- A listener whose session cache had not yet seen a role holder another listener registered a moment ago released the holder's fresh claim as lapsed: a lookup (`GET /v1/roles/<role>`, a role publish) or a role delivery read the claim from the role bucket and the holder from a cache that trails the session bucket, so during a rolling deploy the old task could delete the claim the replacement had just accepted, and the role reached nobody until its holder claimed it again. A soft claim could take such a holder's role, and the role reaper end its claim, the same way. Each now reads the holder from the session bucket itself before it takes anything from it (LEGION-456).
- One MiB of `>` formed 1,048,576 nested quotes inside the document cap and eventually ended the process in a stack overflow while its tree was validated. Dispatch now refuses the document before building that tree (LEGION-465).
- Reading a textblock's inline markdown took one stack frame per nested mark, so the stack and memory it needed grew with the nesting the caller wrote: one 1 MiB upload of 262,140 nested strong marks read with no error but peaked at about 0.9 GB of memory. The inline bound above is checked before any walk of those marks that recurses, and goldmark's own walk through a link's label, which enters every image the label holds, meets each image held to the bound as it is made (LEGION-465).
- A table whose rows hold an escaped pipe in a code span parsed in time quadratic in its size: goldmark's table transformer checked every code span's text against every escaped pipe in the document, and 1 MiB of such rows took over two minutes. Dispatch takes the backslash out of those pipes itself, in one pass, and 1 MiB parses in about two seconds (LEGION-465).
- An id outside nats.go's key alphabet (`ses:bad`) failed whatever met it in the interest and role buckets, since nats.go refuses such a key on every read, write and delete and the stores took that refusal for a failure. A role claim over such a holder, which an earlier build's bare-string claim or a direct bucket write can leave in the role bucket, wrote the claim and then answered 500. A caller could cause the same 500 itself: a session subscribed to a role topic outside the alphabet (`notifications.role.bad:role`, which subscribe accepts) got it on every unsubscribe of that topic, every unsubscribe of all its topics and every `DELETE /v1/interests/<id>`, and its interest stayed. An interest or claim stored under such a key, which only a direct bucket write makes, stopped the interest reaper or the role reaper at that key every five minutes, logging `reaper cycle failed` or `role claim reaper cycle failed` at ERROR. The handle every bucket opens through now names nats.go's refusal as the refusal it is (`bus.ErrInvalidKey`, naming the key), so each of these skips the key as it already skipped one past the key bound: the claim and the unsubscribe answer 200, and the reapers go on, with a WARN naming the key they cannot delete, which an operator removes by hand (`packages/envoy/AGENTS.md`). A `/v1` route given a session id or role outside the alphabet answers 400 naming it, where it answered 500 (503 on subscribe, 404 reading the interests of a session it holds none for), and Dispatch reads a 400 or 413 from `GET /v1/interests/<id>` as no interest, as it reads a 404 (LEGION-456).
- Marking or unmarking a document's text, and checking whether a concurrent change removed the text a write inserted, walked the live tree one stack frame per level with no bound, where an authenticated peer can grow the tree through any number of small websocket updates. Each now refuses a node more than 1,000 levels deep, text included, as the document's reads do, and a peer's update that deepens the tree between a write's read and its transaction is answered `500 DOC_SCHEMA` rather than `500 INTERNAL` (LEGION-465).
- Deleting an element of a live document took one stack frame per level of nesting inside it, so an ordinary delete of a tree an authenticated peer had grown through any number of small websocket updates needed more stack than the goroutine had. Dispatch pins `github.com/reearth/ygo` to the `sjawhar/ygo` fork (at `v1.51.3-sami.1` since the entries below), which walks the deleted children iteratively and carries the transactional GC fix, both open upstream as reearth/ygo#263 and #262 (LEGION-465).
- Dispatch pins `github.com/reearth/ygo` to the `sjawhar/ygo` fork at `v1.51.3-sami.1` (commit
  `e5c1aacf`): upstream v1.51.2 and seven fixes, each open upstream as a reearth/ygo pull
  request - #258 (carried as the fork's own #262), #260, #263, #266, #268, #269 and #291
  (LEGION-535). A browser's document connection now gets a `SyncStatus` frame (Hocuspocus tag 8)
  for every SyncStep2 or Update it sends, in the order it sent them: 1 once the room applied the
  update, 0 when the room refused it (the connection stays open) or the connection is read-only
  and the update adds something the room lacks. ygo defined the tag but never sent it, so the
  dashboard's `@hocuspocus/provider` counted every edit as unsynced for the life of the
  connection. A sync frame that does not decode now closes the connection with 1002 rather than
  being dropped (#291). A room's broadcast of an update Dispatch writes is checked with ygo's
  bundled stores' check, which applies no pending cap, rather than under the server's
  `MaxPendingItems` (#268, which replaces the withdrawn #267): the room has already applied the
  update, and its document still decodes under `MaxPendingItems`. A complete state resolves its
  own dependencies through a worklist rather than a re-scan per step, so a chain-shaped state near
  the pending cap no longer takes time quadratic in its size (#260). A merged update's skip has
  parked the items after it upstream since v1.51.0 (#257). Upstream v1.51.1's depth check on the
  values `YText` stores is the entry below's.
- A mark attribute value nesting exactly 100 arrays and objects, which the schema admitted, is now outside it: ygo stores a mark as one value, the map of its attributes, so that mark reached 101 levels, and from ygo v1.51.1 (reearth/ygo#288) `YText.Insert`, `Format` and `ApplyDelta` panic on a value nested past 100. `pmdoc.Update` writes marks through all three and `MarkRange` through `Format`, so on that ygo an edit rewriting text that carried such a mark answered `500` from a recovered panic. The bound is stated once in terms of ygo's limit (`ygoValueNesting`): 100 for a node's attribute value, which ygo stores as it is, and 99 for a mark's; `MarkRange` checks the mark it is given before it writes. A stored document holding such a mark, which only a crafted client can write, is a tree outside the schema like any other: its reads and edits answer `409 DOC_SCHEMA` naming the repair, settlement writes no version of it and never writes it back, and an upload of replacement markdown repairs it (LEGION-535).
- Dispatch pinned `github.com/reearth/ygo` to the `sjawhar/ygo` fork at `v1.50.1-sami.2` (commit
  `e792b8c7`, on upstream `main` at `4d6865dc`), which adds six ygo fixes to #263 and #262, each
  open upstream (LEGION-496, LEGION-502, LEGION-484). Text no longer changes order when a document
  is encoded again: ygo folded a character into the run before it even when the two were typed
  toward different right-hand neighbours, so a browser joining a room, settlement's copy of a room
  and a compacted state could read the text in a different order from the room (reearth/ygo#266).
  An update that fills a gap in one client's updates is no longer discarded: ygo integrated a
  merged update's items past the gap, and now parks them until the gap arrives (#257). A complete
  document state no longer fails once 100,000 of its items wait on items later in the same state,
  as when a browser whose client id is lower than the server's writes more than 100,000 blocks
  into a paragraph the server wrote: ygo resolves a state's own dependencies before it applies its
  pending cap (#260). A room's broadcast of an update Dispatch writes now validates the update
  with the server's `MaxPendingItems`, the queue the room itself decodes with, rather than ygo's
  default of 100,000 (#267), so a write touching more than 100,000 of a document's existing items
  reaches the room's browsers. ygo's bundled stores keep such an update rather than refusing it
  (#268); Dispatch stores documents through its own store, which already validates under the
  room's queue, so this changes no Dispatch write. A room no browser is in, which only the API
  reads or writes, is now evicted `roomIdleTimeout` (a minute) after the API last touched it, as a
  room its last browser left already was: ygo's `Server.Apply` cleared the room's idle stamp and
  nothing set it again, so such a room stayed in memory until the process exited (#269).

- A document write whose issue closed, or whose server shut down with an editor connected, while
  the committed write was being applied to its room no longer hangs. Closing the room retired its
  persistence worker under the write, and ygo's fallback append, run on the write's own goroutine,
  waited for the write to finish first: the request that made it got no answer, every later write
  to the document waited on its writer slot, and the document's later rooms stopped persisting.
  The room's own update observer now releases the write before that append runs (LEGION-469).
- A write to a document in which a peer had deleted a chain of 200,000 nested blocks, or one whose
  lower-numbered client had written 100,000 items after one ygo could not yet place, failed
  with `fork live document: crdt: invalid update`, and so did every read of such a resident room
  once reads went through a copy. Once its room was evicted or the server restarted, the
  lower-numbered client's document could not be read or opened at all. Every decode of a
  document's state, each copy of its room and its stored history alike, used ygo's default queue
  of 100,000 items parked behind one whose parent it cannot place yet, which such a state
  overruns; each, and the room's own load, now decodes with a queue as long as one update can
  carry. The store's check of each update it appends decoded the update alone at the same
  default, so a browser update of more than 100,000 items written against blocks the document
  already held failed the room and lost the edit; that check now takes the same queue
  (LEGION-469).
- A markdown write of any shape at the 1 MiB cap no longer takes Dispatch down. One 1 MiB upload of
  `)_` held about a gigabyte while it was saved, which killed the 1,024 MiB production task
  (LEGION-481): every element the markdown makes costs memory in the parse, the read-back, the
  render and the live document. A caller's markdown - a spec, an uploaded document or version,
  an edit batch's inserts and replaces, the `find` of an edit and the quote of a comment or ask -
  may now make at most 65,536 elements, weighed as `cmd/dispatch/README.md`'s Element weights
  lists; past that a write is `413 CAP_EXCEEDED`, refused while goldmark parses, naming the line
  where the markdown passes the limit or, where the parse stopped first, the line it stopped reading
  at, and listing the weights; a quote is matched by its text alone. Every
  node the document holds other than a text is an element carrying attributes and costs about what
  two inline nodes do, so each weighs at least two: weighed as one, the heaviest document of
  `<b>a</b>` spans the limit admitted held 213 to 261 MiB to store, of `![](u)` images 327 to 383 MiB
  (and up to 1,044 MiB for four cold reads at once), and of `[^a]` footnote references 262 to
  316 MiB, past the 256 MiB a request may hold; the empty paragraph an empty list item, quote,
  footnote definition or typed block is read as holding weighed nothing, and the heaviest document
  of them held 220 to 236 MiB. An escape - a backslash before punctuation, or a character
  reference, in text outside a code span - makes no node in goldmark's tree, but the document holds
  the character it spells bare, every rendering spells it again, and the renderer reads it back as
  the delimiter or link opener it would be unescaped: a mebibyte of `\~a`, `)\_`, `\)\_` or `\[a`
  weighed four elements and allocated 330 to 720 MiB to parse and 260 to 620 MiB to render, and the
  stored `\)\_` held 256 MiB to read cold and 973 MiB for four reads at once. Every markdown file of
  4 KiB or more among this repository's 618 weighs 1.5 to 379 elements a kibibyte, so prose passes
  to the 1 MiB cap and the densest of them, a comparison matrix, to about 173 KiB; smaller files run
  denser, up to 1,421 a kibibyte for a 147-byte test fixture of empty list items. A paragraph of
  link reference definitions, which goldmark took time quadratic in its lines to read (a mebibyte of
  them took a minute to refuse), is read only as far as its first 1,024 lines.
- A write may no longer grow a stored document past what one upload may hold, measured as an
  upload is measured. Thirty-two 900 KB inserts of prose, each within both limits, grew one document
  to 29.5 MB, on which a one-word edit then held a gigabyte; a few dozen thousand headings' worth
  left one the server could not load (500 on the next write, 503 on every read); and thirty-two
  edits of an ask's options (`PATCH /api/v1/asks/{id}`) grew one to 28.8 MB, on which a one-word
  edit was killed at the production task's 1,024 MiB. Every write a
  caller makes - an upload, an edit batch, an accepted or rejected suggestion, an ask's edited text,
  its answer (`POST /api/v1/asks/{id}/answer`) or resolution, a comment's anchor mark and its margin
  record - runs in a transaction it must join (an unjoined one is refused), and is weighed by the
  markdown it leaves the document storing (what `GET .../text` answers), with an upload's own
  measures: it is `413 CAP_EXCEEDED` when that is longer than 1 MiB and longer than the document's
  was, or makes more than 65,536 elements and more than the document's did, counted as an upload's
  parse counts them (front matter apart, and a table whose short rows would take more padding than
  one write may add counted as past the limit: a 101-column header over 200 one-cell rows measured
  407 elements, the paragraph the parse leaves of it, where written whole it weighs 82,111). One
  that keeps or lowers both passes, so an over-limit document can still be trimmed or split, and any
  document a write leaves can be uploaded again from its own text. An ask's state and who answered
  it and when are the server's, not text a caller writes, so the asks of an over-limit document can
  still be answered and resolved. An answer's words and the options it chooses, whose labels are the
  asker's text, are weighed: where the document has no room for them, the ask takes the whole answer
  and its block is written without it, saying who answered and when, where one choice of a
  1,000,000-character option took a 1 MB document to 8 MB. An ask's
  edited question and options are weighed before they are made into markdown - an element for each
  backslash, `*`, `_`, `~`, backtick, `[`, `]` and `<`, which the markdown escapes or reads as
  syntax, and two for each line feed - and that markdown is parsed on the edit's element budget, so
  text past the 65,536 elements one write may make is `413 CAP_EXCEEDED` before it is rendered,
  where an option description of 400 KB of `)_` used to be taken. Each refusal's message is the
  bound's own, opening `document too large to store`, on every route. A spec, a new document or the
  markdown a rebuild is given (`POST /api/v1/artifacts/{id}/rebuild`) whose stored markdown (its
  rendering, which can run longer than what was sent) is past either limit is refused the same way,
  where a rebuild from the latest version restores it whatever it weighs. A write is also refused
  when the live state it leaves would be past the 1,048,576 structs ygo decodes in one update:
  fourteen alternating versions of 16,000 paragraphs and 16,000 headings, each within both upload
  limits, otherwise left a document every read answered 500. A document's margin - the record of
  every comment and suggestion it has had, which every load
  builds though no rendering carries it - is weighed too: one record (a comment's body and replies,
  a suggestion's replacement) holds at most 256 KiB of text and the margin 1 MiB, and a comment,
  suggestion, reply or edit that would leave either past its bound and bigger is `413
  CAP_EXCEEDED`, naming the bound and both sizes; a status change (accept, reject, resolve) is not
  text a caller writes and passes. Thirty-two open suggestions of 900 KB took one cold websocket
  load to 296 MiB, and sixty-four four cold reads at once to 1,223 MiB. The marks a document's
  comments, suggestions and asks hold on its text carry at most 1 MiB of ids and authors, which no
  rendering carries either: an anchored comment, suggestion or ask that would leave them past it and
  carrying more is `413 CAP_EXCEEDED`, where thirty asks anchored by a session whose id was 200 KB
  left a 210-byte rendering over six megabytes of live document. An answer is stored on its ask as
  well as in its block, and settlement writes it back into a block that returns to the document:
  that is weighed the same way, each returning answer on its own in document order, and an answer
  that would leave the document past either limit and bigger stays on its ask alone, its block
  restored to its state and who answered, until the document has room for it. Twenty-four answers
  of 900 KB returned by one 1,540-byte edit had left a 21.6 MB document whose cold text read held
  368 MiB.
  Browser edits over the websocket are applied before any check and are not bounded by
  this (LEGION-487). A document's stored history is bounded too: compaction runs as a room
  closes, as the server shuts down, and daily, folding every update into the document's state
  (LEGION-496), so repeated uploads of a version or replies to one comment, whose margin record
  each reply rewrites whole, cost a load only for as long as a room stays open between those -
  the same scenario that left 257 MB stored under 500 replies of 2,000 characters, with a
  one-word edit holding 1,842 MiB, before LEGION-496 now compacts to the document's own state.
- Document settlement no longer undoes an edit a browser or an agent makes while it settles
  (LEGION-479). Settlement wrote its repairs (the block ids it stamps, an ask block's server-owned
  attributes it restores) as the tree it had read before its database work, so an edit made in
  between was reverted in the live document for every connected browser, and could reach the
  stored document half applied. Each repair is now read and written in one transaction on the
  document as it stands, and so is each stamp `envoy-dispatch backfill-block-ids` writes. The
  version a repairing settlement writes is the document after its repairs, so it holds such an
  edit and credits the edit's author, whose own settlement then writes no second version. A
  repair settlement wrote into the room is committed even when the document moved after the
  write; before, it was dropped from the store while the room and its browsers kept it. A
  settlement whose room was closed and loaded again during its database work (its last browser
  left) writes nothing into the reloaded room, whose own settlement makes the repair, and logs
  `dispatch: document settlement wrote nothing into a replaced room` at INFO.
- A settlement no longer leaves a suppression slot queued that nothing releases (LEGION-479). A
  stamp that found the ids already repaired once it read the document kept its slot, and the
  room's persistence worker waited on that slot at the room's next update, so the room stopped
  storing updates. A settlement that both stamps ids and repairs an ask block held its two updates
  with one slot that matched neither, so both were stored twice and the slot stayed queued. Each
  update now has a slot of its own, and every path a repair takes releases its slot.
- A document settlement whose room closed while its repair committed (its last browser left, or
  the server shut down) no longer hangs for good (LEGION-479). Closing the document's issue never
  closes the room under a repair: the settlement holds the issue's row until it commits, and the
  close waits for that row. When the room closes under the repair, ygo stores the repair's update
  on the settlement's own goroutine once the room's persistence worker has exited, and neither
  wait there ended: the store waited for the settlement to release that update's suppression slot,
  which it does only after the store returns, and the worker's exit compaction waited for the
  room's lock, which the settlement held. Until the server restarted the document did not settle
  and none of its later updates were stored, since every update a document stores takes that lock.
  Every close of the room the server makes now waits for the repair to reach the room's
  persistence (LEGION-498, below), and the room worker's compaction skips a room whose lock is
  held, except when it evicts a failed room. The same slot wait held
  `envoy-dispatch backfill-block-ids` for good at a document whose room closed under its stamp.
- A document whose room failed while a settlement wrote a repair into it (a block id it stamps, an
  ask block's attributes it restores) no longer stays failed until the server restarts, with every
  read and write that waited for its recovery hung and none of its updates stored (LEGION-498). The
  failed room's eviction waited for the document's lock, which the settlement held, while the
  settlement waited for the room's persistence worker that the eviction had retired. A room the
  server closed under a settlement's repair while a second write committed into it, such as a
  published edit, hung the same way, each write's store waiting on the other's. The server's own
  closes of a room (a failed room's eviction, an issue's close, a shutdown's close of a room with
  an editor) now wait until a repair being written into the room has reached its persistence, and
  a repair that meets a close under way writes nothing, as one whose room was replaced does. An
  edit that reached the room's persistence just ahead of a settlement's repair is no longer dropped
  in the repair's place when the settlement discards the repair because the issue closed or the
  server began stopping.
- A document version no longer drops the credit of an edit it does not hold (LEGION-503). A
  version's commit released its authors by key, so an author already pending when the version
  took its authors who edited again before it committed lost the second edit's credit too, and the
  version holding that edit credited nobody for it: a settlement's commit did so when the edit
  landed while its update observer was between crediting and arming its settlement, and a named
  version's or snapshot's commit whenever the edit landed while the transaction held the writer
  slot. A named version or snapshot over a transaction's own write also took its authors after it
  read its tree, so an edit made in between was credited on that version, which lacked it, and on
  no other. An edit a version's tree held before its update observer had credited it (ygo runs the
  observer once the edit's transaction has released the document, and observers wait for each
  other's renders) was credited on no version: the edit's own settlement found the document
  versioned, wrote none, and released the author. Each pending author now carries the change it
  credits, every version takes its authors no later than it reads the tree it records, a version's
  commit releases only entries credited through that take, and a settlement that writes no
  version releases nothing, so the next version credits such an author. That includes an author
  whose edits came to nothing, typed and undone before a settlement. An upload that changes the
  document clears every credit pending at its write's room read, whether its replacement removed
  that edit or kept it, and its version credits its uploader alone; an edit credited after that
  read stays pending for the next version, and an upload that changes nothing clears nothing. A new
  ask is attributed to whoever introduced its block: a service edit's, an upload's or a committed
  transaction's own before/after trees name the ids it adds, staged on the write and registered
  into the room's bookkeeping only once the write commits, never before - discarding the
  transaction, or refusing the write for any other reason, leaves no trace, so a later, separately
  committed write of the same author-chosen literal id is never outranked by one that never reached
  the room. Registration happens before the update can reach any observer, rather than
  whichever update's observer happens to render a merged catch-up first; an id no write registered
  this way is a browser's, named for the one browser connected when its update arrived, or the
  document-settlement actor when several were. A settlement's or the block-id backfill's own id
  repair, and an edit's repair of an existing, unrelated block, each carry forward the author
  recorded for the id a rename replaces, unless it is a copy of the block that keeps that id - on a
  write joined to a transaction too, where the write's own generic before/after diff would
  otherwise count the renamed id as newly added and claim it for the write's own actor instead. A
  copy's previous id stays live in the room's own bookkeeping, since another block still carries
  it; any other rename's does not, closing a window where a later block reusing that literal id
  could be mistaken for the one just retired. A rename whose own update's observer had not yet
  recorded an author is named after the settlement's own actor instead. Before, a settlement named
  its own actor - the latest editor, or the first pending author - so a browser edit elsewhere
  could take an agent's ask, and a block that arrived while the settlement ran could take the name
  of an earlier editor; later, two updates landing while one's own observer renders a catch-up that
  includes both still let whichever ran first claim both blocks; later still, a discarded write's
  registration could outrank a later write's legitimate one, and a conditional edit's own repair
  of a copied or unrecorded rename could still be claimed by the write's own actor instead of
  falling through correctly. A block whose update the observer has not rendered yet waits for the
  settlement that observer arms; a block the room held when it loaded is still named as the
  settlement's other events are. Approval moves name the actor whose edit moved the version, even
  when it credits several authors, so a stale pending author does not make a human's move appear as
  the document settlement or suppress its notification.
- A GitHub App response over 1 MiB now fails whole instead of returning a truncated body. The
  dashboard proxy answers `502 GITHUB_UPSTREAM` and names the 1 MiB limit.
- Saving a document, comment, ask, or message with a long run of underscore-joined characters
  no longer takes quadratic time in Postgres search indexing. `pmdoc` also avoids quadratic work
  in Goldmark's email and delimiter scans and in renderer closer scans. A document that exceeds
  one update's 1,048,576-item cap is now refused as `413 CAP_EXCEEDED` with its item count instead
  of `500` (LEGION-465).
- A Dispatch shutdown no longer drops a document settlement its budget cuts short. The settlement
  was armed only in memory, so a deploy that stopped the server while a large document settled
  (a 1 MiB `a_b*` document took 4.5-6.8 s at load 90-170, past the 5 s budget) left its ask blocks
  unindexed and an edit's version unwritten until the next edit. Every durable document update
  now records the settlement it owes in `doc_settlements_pending` (migration 0063), which the
  settlement deletes when it commits. A room's load settles a document with that row, and the
  server arms the settlement of every open document whose row is a minute old, at start and each
  minute, so one nobody opens settles too. Shutdown settles only the documents that owe one,
  cancels a settlement past its budget so its transaction rolls back, and logs per document
  whether it settled or was left to resume (LEGION-465).
- Dispatch indexes a mention written in markdown (LEGION-463): `**dispatch://KEY**`,
  `` `dispatch://KEY` ``, `_dispatch://KEY_`, `~~dispatch://KEY~~`,
  `[dispatch://KEY](dispatch://KEY)` (how a document stores `<dispatch://KEY>`), a bracketed
  dashboard URL, a reference in a table cell written without padding (`|dispatch://KEY|`), and a
  reference followed by a no-break or ideographic space each write their `mentions` edge. A
  reference ends at whitespace, an angle or square bracket, a quote, a backtick or a pipe, except
  the bracketed host of an IPv6 dashboard URL, and the sentence punctuation, emphasis delimiters
  and unbalanced `)` after it are dropped in one pass, where a spec of one reference and 900,000
  `)` took 21 s to save. The server parses a reference as the dashboard does: a query's pairs
  split on `&` alone, an item id decoded, a slug only whole, a version without a leading zero, and
  nothing from a reference holding a control character or an item id that decodes to one. A body
  citing `…/spec?comment=%00` was answered 500, since Postgres refuses a NUL in the index, and is
  now stored. The dashboard's composer pills, unfurl cards and linked text read text by the same
  rule. Text stored before the deploy gets its edges from `envoy-dispatch rebuild-refs`.
- The dashboard no longer hangs on a comment or an ask question made of repeated `http://`. The
  check that shows an unfurl card only for a body of nothing but references tried every way to
  split the body into references, which in Chromium ran for minutes on a 2,000-character comment.
- During a rolling deploy the replacement listener serves `/v1` and runs its role lane once NATS
  is connected and the interest and session caches are warm (about 100 ms after it listens),
  instead of answering every `/v1` call `503 service starting` until the old task let go of the
  durable consumer (84 s in production on 2026-10-01). The durable's bind is polled every 2 s and
  lands within 2 s of the old task's exit, still giving up after 135 s with
  `subscribe failed after max attempts, shutting down`; `/healthz` stays 200 `starting` until it
  binds. Both tasks of a machine are one role-lane queue group, so the overlap forwards no role
  message twice. A SIGTERM during the bind wait is an ordered shutdown. The bus now restores every
  subscription after a reconnect even when one cannot be restored, so a role lane is not left down
  while the durable still refuses the bind (LEGION-456).
- Dispatch exits with status 1 when it cannot bind its listen address. It logged
  `dispatch: listen … bind: address already in use` and exited 0, so a supervisor read a port
  clash as a clean stop.

- A Dispatch request refused because its document room failed answers
  `503 DOC_SERVICE_UNAVAILABLE` whatever failed the room, as a comment's or ask's
  `anchor_block_error` names it: both name a document error through one classification
  (`api.documentErrorCode`), which takes a failed room before any cause the room carries. A room
  failed by settlement's schema refusal answered `500 DOC_SCHEMA`, though the request had not met
  that refusal itself; a retry once the room is evicted meets the document (LEGION-460).

- A search that contains only stop words now returns `200` with no results, so every consumer
  can show an empty result rather than a retryable failure.

- A targeted message's delivery claim no longer deadlocks with the session's reply to the same
  message: it takes the message row `FOR NO KEY UPDATE`, as the new accept does, which the
  reply's foreign-key `FOR KEY SHARE` does not wait for (LEGION-394).
- Dispatch no longer stores a table row without its last cells. A cell ends at every `|` not
  written `\|`, inside code and links too, and the parser dropped the cells a body row held past
  its delimiter row's, which the browser editor's parser keeps: a spec, upload or version whose
  code span held a bare `|` in a row that filled its table was stored with the rest of that row
  gone, and no refusal. Such markdown is now refused (`400 INVALID_MARKDOWN`; an insert's
  `markdown` answers `INVALID_OP`), naming the row's cells, its table's width and its opening
  words, and both fixes: write a `|` inside a cell as `\|`, or give the header and delimiter rows
  as many cells as the row. A row whose cells past the width are all blank is still read at the
  table's width. A bare-row insert now decides in three steps. A fragment whose every line yields
  a cell and an unescaped `|` (a lone `|` yields no cell, and a line whose only pipe is `\|` has no
  separator), with no line delimiter-shaped (cells of three hyphens or more), is parsed under a
  header at the target table's width. What that parse refuses is refused, a wide row of that
  table as `TABLE_WIDTH`. When it refuses nothing and reads one table holding every line, those
  rows are inserted. Every other fragment goes to the block path whole, which reads it as a
  document of its own. The insert used to count cells itself first, line by line, over a fragment
  trimmed of Unicode spaces and a first row's indentation, so it now answers differently in these
  ways. It drops blank cells past the width and refuses text there, where it refused both. A `|`
  after an even run of backslashes is text to it, as it always was to uploads, so
  `| A11 | new \\| extra |` under two columns is stored as one cell reading `new \| extra` where it
  was refused as `TABLE_WIDTH`; the browser editor's reading of such a pipe is LEGION-412. A space
  outside ASCII, a vertical tab or a form feed is a cell's text wherever it stands, so
  `| A11 | new |` then U+00A0 is refused as three cells where it was stored as two, and a line of
  `|` then U+00A0 is a row holding it, as an upload reads it, where it was a lone `|` that sent the
  fragment to blocks. A line the parser refuses as another block (indented code, `2. | a | b |`)
  is refused as an upload refuses it, `INVALID_OP` on `markdown`, however many cells it holds and
  on any row. A fragment that goes to the block path was refused as `TABLE_WIDTH` wherever
  the old count met a line too wide before whatever sends the fragment there, as with
  `- | a | b |` (counted as three cells), or `| A11 | x | y |` then a lone `|` or a line whose only
  pipe is `\|` under two columns; it is now written as the blocks it reads as.
  The route also tries a fragment as rows before it reads it as a document of its own, so rows
  that reading refused, such as `[x]: |`, which it took for a link reference definition, are
  inserted as the rows an upload reads. Versions written before 2026-09-19 hold rows with text
  past their table's width, since the renderer then wrote a code span's pipe unescaped;
  re-uploading one is refused rather than stored short. A row's closing `|` after an odd run of
  backslashes (`| x | y \|`) is now kept as the last cell's text, as the browser editor reads it,
  where it was dropped (`y \`). An image's alt holding a backslash before a pipe in a table cell
  is written so that the browser editor reads it as one cell too.

- `GET /api/v1/broadcasts/{id}` now returns recipient copies in the order the sender named them,
  including the relative order of recipients left after exclusions. Broadcasts created before
  this ordering was stored retain their existing timestamp-and-UUID fallback order.

- A listener restart no longer replays the `envoy_interests` bucket's delete markers before it is ready, because each listener now collects them. Every delete path left one (the reaper for each dead session, an unsubscribe-all, the admin delete) and nothing ever removed one: production's bucket held 41,873 subjects for 40 live keys, and the on-prem listeners spent 17-25 s of every restart with no deliveries. A pass reads the stream, scans the bucket MetaOnly, takes the lowest revision that scan delivered as a PUT, reads the stream again and purges it below that floor — a floor from the stream, not from the cache, so a live key this build cannot decode is protected and a partial cache cannot raise it. It purges only when the scan completed, it saw a live key, the second read is the stream the first read read (the same creation time), the sequence space only moved forward between them, and `FirstSeq < floor <= LastSeq`; otherwise it refuses, at WARN when a read failed, the scan did not complete, the stream was replaced, the sequence space moved backward, or the floor is above the stream's last sequence, and at DEBUG when there is nothing below the floor or no live key at all. Every pass logs every stream value the decision used (`read1_*`, `read2_*`, `floor`, `live`, `delete_markers`), plus `purged`, `purged_first_seq` and `purged_msgs` when it purged; `purged` is approximate both ways, since a concurrent put under-reports it and a peer listener's purge in the same moment is counted by both passes. Every listener runs it after its interest cache's first warm-up and then every five minutes; the purge is idempotent, so no leader is needed. Measured with the listener binary on a bucket seeded with 500 markers behind 3 live keys: `purged=500`, the bucket down from 503 subjects to 3, every live key at its stored revision, and the next restart's warm-up line reporting `delete_markers=0`. It adds one NATS capability, publish on `$JS.API.STREAM.PURGE.KV_envoy_interests`; a user without it leaves every marker in place, logs `interest marker collection could not purge the bucket` once and keeps serving.
- A listener that mounts no GitHub webhook route (`ENVOY_WEBHOOKS` without `github`, as on the on-prem fleet) no longer opens the CI store: it creates no `envoy_ci_state` watch or bucket and runs no CI summary loop, and `/healthz` reports `"ci_cache": "not_applicable"`. The watch's initial scan of every record (62 MB in production) over a Tailscale DERP relay held up the listener's other startup requests past their 10 s deadline, so an on-prem listener that restarted either exited (`context deadline exceeded`) or stayed unhealthy and delivered nothing. CI settlements are published by the listeners that receive GitHub webhooks; one that mounts the route behaves as before.
- A listener restart no longer spends one NATS round trip per stored role claim before it is ready. `store.Open` snapshots every claim's revision — the grace a restored holder gets to register again — from one watch over the `envoy_roles` bucket's existing keys, as the interest, session and CI caches read theirs, instead of listing the keys and then reading each one. Measured with the listener binary against a link relayed at about 40 ms round trip and a bucket holding 766 claims: 35.6 s from `webhooks open` to a healthy `/healthz` before, 1.2 s after, against 0.9 s with an empty bucket. Restored claims keep today's behaviour, including the grace window and the claim stored under a key this build cannot read. It also closes a defect the per-key read had: a scan that does not reach the end of the bucket now fails the start naming how many keys it read, rather than opening on a partial snapshot whose missing holders lose their grace a session TTL early. nats.go ends a scan early two ways and only one looks like a failure — it closes the updates channel when the watcher's subscription ends, and on its own idle timeout (the JetStream `MaxWait`, 10 s here) it sends the same marker a complete scan ends with, reporting the timeout only on `Error()`, which the scan now reads at the marker. `nats.KeyValue.Keys()` carries that timer too, so a listener on the released build restores a partial snapshot over a link that goes quiet for the `MaxWait` and then recovers: measured against a link held 13 s mid-scan, 232 of 400 claims restored with no error. The fix reaches production with the deployment's listener pin bump that carries this build.
- A role claim that overtook another session no longer risks deleting a newer claim made in the same instant; old-holder cleanup now touches only the previous holder's topics, never the role row.
- Document version mutations now emit one retained, non-notifying `comment.anchor_refreshed` or `ask.anchor_refreshed` event for each changed open anchor, so stream consumers immediately observe orphaned quotes.
- A webhook redelivery of an event the notification stream already holds no longer publishes a second copy. Webhook envelopes whose dedupe key is the source plus the upstream delivery id (GitHub's `X-GitHub-Delivery`, Slack's `event_id`, Ghost Wispr's `X-GhostWispr-Delivery`) now publish under a JetStream MsgId of that key and topic, the rule Dispatch envelopes already used. Other producers that publish under those source names - CI settlements, the MCP bridge and `/v1/messages/*` callers - still publish every time. GitHub documents that a redelivery carries the original `X-GitHub-Delivery`, and Slack retries under the original `event_id`; that Ghost Wispr resends under its original delivery id is assumed, not documented.
- A GitHub delivery no longer makes an envelope larger than NATS takes, which a body up to the 25 MiB cap could: every text a webhook envelope copies is capped at 2,048 runes, a push's changed paths at 32,768 runes as well as 100 paths, and every text a check run copies into the CI store at 2,048 runes, a name past that keeping a digest of the whole name so two names sharing their first 2,048 runes stay two checks; a ref or workflow file name writes whitespace, `*` and `>` as `_` in its topic. What NATS still refuses however often it is sent (a subject past the server's 4 KiB protocol line, over which the server closed the listener's NATS connection, a message past the server's max payload, or a subject holding whitespace or an empty token) is answered 422 and logged `<source> publish refused` rather than 503 and `<source> publish failed`, so Dispatch's sweep does not redeliver it and the publish-failure line does not fire. A head's CI record is bounded at 384 KiB and its settlement at 960 KiB; a check past either is refused the same way and that head never settles from the listener, and a settlement NATS refuses anyway marks its head the same way instead of being published again every second. `/v1/messages/publish` and `/v1/messages/send` answer a message NATS cannot take whole with 413 naming its size. Every KV handle checks its keys before sending (a key long enough to close the connection, or holding an empty token, is refused), so a `/v1` session id or role NATS would refuse is a 413 or 400 rather than a 503 after the listener's NATS connection closed. A write is held to the longest subject its key makes; a key an earlier build stored past that bound still lists and deletes, the stores skip it where they cannot read it, and the reapers delete it, so such a key never stops a listener from starting. Dispatch's outbox logs a destination NATS refuses once and counts it done instead of retrying it forever.
