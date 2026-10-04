# Changelog

## [Unreleased]

### Added

- `DISPATCH_AGENT_TOKEN` takes several values separated by whitespace, the first the current one, so the shared agent token can rotate with an overlap: the HTTP API and the document websocket accept every value, comparing a bearer with each in constant time. Startup refuses an empty entry (two whitespace characters in a row) or a repeated one, naming its position and never its value; one value behaves as before. A request that authenticates with a value after the first logs `dispatch: request authenticated with a previous shared agent token` at WARN, with the value's position, the rightmost `X-Forwarded-For` address (the connection's own without one), the User-Agent and the path, at most once per address and User-Agent every 10 minutes (LEGION-538).
- `envoy-dispatch settings` prints every Dispatch setting the server and its subcommands read, one row each with its `_FILE` form, default, whether it is required and a one-line description, from one table (`cmd/dispatch/settings.go`) that is now the only place Dispatch's own code reads its environment; the docs site's Dispatch configuration reference is generated from it. The libraries Dispatch links still read their own variables (`HOME`, libpq's `PG*`, Go's proxy, certificate and runtime variables), which the table does not list. Every setting resolves as before, the `envoy.json` overrides and `_FILE` forms included; the readers that used to call `os.Getenv` themselves (the dashboard directory, the GitHub App credentials, the signing key and insecure-cookie flag, the `envoy.json` overrides, the Envoy listener token, and NATS's reach and nkey) are handed their value from the table.
- `envoy-dispatch routes` prints the route table `GET /api/v1` serves, read without a database or a listener; the docs site's Dispatch HTTP API reference is generated from it.
- `envoy-dispatch census`: the pre-deploy census of the migrations a database has not recorded (the runner's own rule), for a deployment to run before it rolls the service. It prints, for each pending migration, the tables it locks above ACCESS SHARE with size, row count and the sessions holding locks on them (pid, role, application, state and transaction age, "not visible" where Postgres hides one, or "autovacuum worker"; never query text), the transactions open longer than a minute or of an age it cannot see, and the count the migration's own `<version>_<name>.census.sql` answers. Those tables are the ones its statements name (an aliased `update` and a foreign key's referenced table among them; none in a comment, a string literal or the body of a function the migration defines), the table behind an index it drops or alters, and every table a foreign key reaches from rows it writes (a cascade, or a check that a row is still referenced), read from `pg_constraint` and from the keys earlier pending migrations add, inside a `DO` block too, and checked for holders and readability but not counted. It refuses (exit 1) a census that counts rows; a table a migration names above 1 GiB, which it does not count; a lock on a touched table held by a transaction older than a minute, or by one whose age Postgres hides from the census's role (granting that role `pg_read_all_stats` lets it read the age) or does not record (`track_activities` off), but not by an autovacuum, which Postgres cancels for the migration, unless it is anti-wraparound, or may be (its activity hidden or untracked, its table past its freeze age), or `deadlock_timeout` is not shorter than the lock timeout; a touched table it cannot read within the five-second lock timeout or the statement timeout; a census that fails, a name the database lacks included, even behind an earlier pending migration that may create it; and a database that records no version yet holds tables. A fresh database, one recording no version and holding no table, passes with `census: fresh database, nothing to check`. A failure names the file and the SQLSTATE, and Postgres's message only when it points into the census's own text, so no row value reaches a log. The census exits 2 when it cannot be taken (a connection whose `search_path` names no schema that exists among the causes), and writes nothing: one read-only transaction, the census statement sent through the extended protocol whatever the connection string asks. `pgmigrate.Load` refuses a census that is not one select, that holds a Unicode escape (`U&'…'`, `U&"…"`), or that names `pg_terminate_backend`, `pg_cancel_backend`, `pg_sleep`, an advisory-lock function (`pg_try_advisory_*` included) or a function that runs a query given as text (`query_to_xml` and its kin, `ts_stat`, `ts_rewrite`), bare or quoted and in any case, anywhere outside its comments and string literals, which it finds as Postgres 16's lexer does; the census runs with `standard_conforming_strings` on, so Postgres reads its literals the same way. Every migration from `censusRequiredFrom` (56, `internal/dispatch/store/store_test.go`) declares a census; 0053, 0054 and 0055 carry worked examples. Each store's tests hold the census's reading of every migration to the locks it really takes, run every shipped census at the schema just before its migration, and require a census that reads what a migration from `censusRequiredFrom` on creates, renames or gives a new type to name that migration, since a release carrying both refuses every deploy. An unknown `envoy-dispatch` subcommand now exits 2 instead of serving, which migrated the database (LEGION-459).
- `DISPATCH_DEV_SIGNIN=1` mounts `GET /auth/_dev/signin?login=<login>&next=<path>` on Dispatch, which signs an allowlisted login in with the same session cookie a GitHub sign-in issues, with no GitHub step, so a local instance can be driven signed-in. Boot refuses the flag unless identity is cookie, the listen address is a loopback IP literal, the dashboard origin names `127.0.0.1`, `[::1]` or `localhost`, every `DATABASE_URL` host is loopback or a unix socket, `DISPATCH_SIGNING_KEY` is unset, `ENVOY_ALLOW_REMOTE_NATS=1` is not set while NATS is on, a set `DISPATCH_AGENT_SECRETS_URL` names a loopback host, `ENVOY_URL` names a loopback host, and a loaded GitHub App private key comes from `DISPATCH_APP_PEM_B64` with `DISPATCH_GITHUB_API_BASE` naming a loopback host, since a signed-in session can have the App probe and import any repository it is installed on. A key in `app.json`, where a developer keeps the real App's key, is refused whatever the base, naming the file; the environment's key must be a throwaway, as `packages/dispatch/e2e/run-server.sh` generates one, because every App call hands a signed App JWT to whatever listens at that base. These refusals come before NATS or Postgres is dialled. The signing key is then generated per process, so a minted cookie is worthless on any other server and dies with the process. What a signed-in session writes to the database (a `dsp_` token it mints, the session and token rows its sign-out changes) is honoured by every server on that database, so a dev-sign-in server needs a database of its own. While the flag is on, every request must carry the dashboard origin as its `Host` (`421 HOST_MISMATCH`), the route serves only a loopback peer with no forwarding header (`403 DEV_SIGNIN_FORBIDDEN`) and logs every mint at WARN, and the GitHub proxy answers `503 GITHUB_TOKEN_UNAVAILABLE` without reading a stored token pair. `DISPATCH_LISTEN_HOST` may now be an IPv6 literal with or without brackets: `::1` listened on `::1:8766` and failed with "too many colons", and now listens on `[::1]:8766`. A bad `DISPATCH_PORT` now refuses the boot before NATS and Postgres connect.
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

### Changed

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
  one the session took between a run's last queue or aside poll and its `agent_end` reaches the
  stream untagged and shows twice (`packages/pi-envoy/AGENTS.md`). The Agents page shows an
  attempt the session accepted as delivered to the session's
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

- A Markdown document now nests at most 100 blocks, and a document tree with a node more than 1,000 levels below the document, or an attribute value nesting more than 100 arrays and objects, is outside the Proof schema (LEGION-465). The bounds sit where every read serves the tree: past about 5,000 levels the document token is JSON that `encoding/json` will not write from Go 1.27 or read in any version, and `GET /blocks`, which hashes each block's subtree apart, does work growing with the square of the depth. A live tree past either tree bound is treated as any other tree outside the schema: settlement writes no version, its reads and edits answer `409 DOC_SCHEMA` naming the repair, the document websocket refuses it, and an upload of replacement markdown repairs it (LEGION-469). A textblock's inline markdown nests at most 100 marks inside one another - emphasis, strong, strikethrough, links, images and code - and deeper content is refused naming the line. An accepted suggestion whose own markdown nests within 100 blocks but lands deep enough that the document would nest past them is refused as `400 INVALID_OP` on `replace_with`, naming how many blocks the result nests (LEGION-465).

### Fixed
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
- Deleting an element of a live document took one stack frame per level of nesting inside it, so an ordinary delete of a tree an authenticated peer had grown through any number of small websocket updates needed more stack than the goroutine had. Dispatch pins `github.com/reearth/ygo` to the `sjawhar/ygo` fork (at `v1.50.1-sami.2` since the entry below), which walks the deleted children iteratively and carries the transactional GC fix, both open upstream as reearth/ygo#263 and #262 (LEGION-465).
- Dispatch pins `github.com/reearth/ygo` to the `sjawhar/ygo` fork at `v1.50.1-sami.2` (commit
  `e792b8c7`, on upstream `main` at `4d6865dc`), which adds six ygo fixes to the two above, each
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
