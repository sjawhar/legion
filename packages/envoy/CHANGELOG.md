# Changelog

## [Unreleased]

### Added

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

### Changed

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

### Fixed
- Dispatch exits with status 1 when it cannot bind its listen address. It logged
  `dispatch: listen … bind: address already in use` and exited 0, so a supervisor read a port
  clash as a clean stop.

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
- A listener restart no longer spends one NATS round trip per stored role claim before it is ready. `store.Open` snapshots every claim's revision — the grace a restored holder gets to register again — from one watch over the `envoy_roles` bucket's existing keys, as the interest, session and CI caches read theirs, instead of listing the keys and then reading each one. Measured with the listener binary against a link relayed at about 40 ms round trip and a bucket holding 766 claims: 35.6 s from `webhooks open` to a healthy `/healthz` before, 1.2 s after, against 0.9 s with an empty bucket. Restored claims keep today's behaviour, including the grace window and the claim stored under a key this build cannot read. It also closes a defect the per-key read had: a scan that does not reach the end of the bucket now fails the start naming how many keys it read, rather than opening on a partial snapshot whose missing holders lose their grace a session TTL early. nats.go ends a scan early two ways and only one looks like a failure — it closes the updates channel when the watcher's subscription ends, and on its own idle timeout (the JetStream `MaxWait`, 10 s here) it sends the same marker a complete scan ends with, reporting the timeout only on `Error()`, which the scan now reads at the marker. `nats.KeyValue.Keys()` carries that timer too, so a listener on the released build restores a partial snapshot over a link that goes quiet for the `MaxWait` and then recovers: measured against a link held 13 s mid-scan, 232 of 400 claims restored with no error. The fix reaches production with the agent-c listener pin bump that carries this build.
- A role claim that overtook another session no longer risks deleting a newer claim made in the same instant; old-holder cleanup now touches only the previous holder's topics, never the role row.
- Document version mutations now emit one retained, non-notifying `comment.anchor_refreshed` or `ask.anchor_refreshed` event for each changed open anchor, so stream consumers immediately observe orphaned quotes.
- A webhook redelivery of an event the notification stream already holds no longer publishes a second copy. Webhook envelopes whose dedupe key is the source plus the upstream delivery id (GitHub's `X-GitHub-Delivery`, Slack's `event_id`, Ghost Wispr's `X-GhostWispr-Delivery`) now publish under a JetStream MsgId of that key and topic, the rule Dispatch envelopes already used. Other producers that publish under those source names - CI settlements, the MCP bridge and `/v1/messages/*` callers - still publish every time. GitHub documents that a redelivery carries the original `X-GitHub-Delivery`, and Slack retries under the original `event_id`; that Ghost Wispr resends under its original delivery id is assumed, not documented.
- A GitHub delivery no longer makes an envelope larger than NATS takes, which a body up to the 25 MiB cap could: every text a webhook envelope copies is capped at 2,048 runes, a push's changed paths at 32,768 runes as well as 100 paths, and every text a check run copies into the CI store at 2,048 runes, a name past that keeping a digest of the whole name so two names sharing their first 2,048 runes stay two checks; a ref or workflow file name writes whitespace, `*` and `>` as `_` in its topic. What NATS still refuses however often it is sent (a subject past the server's 4 KiB protocol line, over which the server closed the listener's NATS connection, a message past the server's max payload, or a subject holding whitespace or an empty token) is answered 422 and logged `<source> publish refused` rather than 503 and `<source> publish failed`, so Dispatch's sweep does not redeliver it and the publish-failure line does not fire. A head's CI record is bounded at 384 KiB and its settlement at 960 KiB; a check past either is refused the same way and that head never settles from the listener, and a settlement NATS refuses anyway marks its head the same way instead of being published again every second. `/v1/messages/publish` and `/v1/messages/send` answer a message NATS cannot take whole with 413 naming its size. Every KV handle checks its keys before sending (a key long enough to close the connection, or holding an empty token, is refused), so a `/v1` session id or role NATS would refuse is a 413 or 400 rather than a 503 after the listener's NATS connection closed. A write is held to the longest subject its key makes; a key an earlier build stored past that bound still lists and deletes, the stores skip it where they cannot read it, and the reapers delete it, so such a key never stops a listener from starting. Dispatch's outbox logs a destination NATS refuses once and counts it done instead of retrying it forever.
