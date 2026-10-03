# Envoy Package

Go-based cross-machine event transport and delivery subsystem.

## Overview

Envoy owns transport, routing, delivery, and the native Dispatch event path:

- ingests Slack, GitHub, Ghost Wispr, and agent events
- publishes ordinary notifications through JetStream and role lanes through core NATS
- resolves target OpenCode sessions
- delivers by hot `prompt_async` (ordinary events stay in JetStream for retry if a session is unavailable)
  publishes retained `notifications.dispatch.issue.<KEY>.<type>`,
  `notifications.dispatch.document.<PROJECT>.<slug>.<type>`, and
  `notifications.dispatch.project.<PROJECT>.<type>` envelopes

It does not own Legion workflow policy. The daemon/controller decides what to do; Envoy moves
events to the right session.

## Where to look

| Task                   | Location                                  | Notes                                              |
| ---------------------- | ----------------------------------------- | -------------------------------------------------- |
| Webhook handlers       | `internal/webhook/{github,slack,ghostwispr}.go` | HTTP ingress, signature verification, publish path |
| Webhook config         | `internal/webhook/config.go`                     | ENVOY_WEBHOOKS parsing, startup validation         |
| Listener behavior      | `cmd/listener/main.go`                    | subscribe/match/deliver flow                       |
| NATS client            | `internal/bus/nats.go`, `internal/bus/recovery.go` | connect, subscribe and publish; reconnect, recovery and the reconnect hooks |
| NATS credential        | `internal/bus/nkey.go`                    | every bus connection's nkey user: `NATS_NKEY_SEED_FILE` (wins) or `NATS_NKEY_SEED`; unusable refuses, neither set connects without one. `Connect` and `ConnectOwningStream` read these and `ENVOY_ALLOW_REMOTE_NATS` from the process environment, or from the lookup `bus.WithEnvironment` hands them; `Dial` from the lookup its caller passes. envoy-dispatch hands both its settings table |
| Stream definition      | `internal/bus/stream.go`                  | `ENVOY_NOTIFICATIONS` subjects, retention and duplicate window, and their reconciliation at start |
| Session delivery       | `internal/session/session.go`             | hot delivery via prompt_async                      |
| Interest storage       | `internal/store/kv.go`                    | JetStream KV subscriptions                         |
| KV cache watchers      | `internal/kvwatch/kvwatch.go`, `internal/kvwatch/scan.go` | the interest, session and CI caches' one watch lifecycle: first start, readiness, rewatch, recreated buckets, stop; and the one-shot scan of a bucket's existing keys (`ScanExistingKeys`); `kvwatchtest` fakes nats.go's scan idle timeout |
| Topic matching         | `internal/routing/match.go`               | wildcard matching                                  |
| Envelope normalization | `internal/contracts/*.go`                 | generated contract + source-specific normalization |
| Native Dispatch workspace | `cmd/dispatch/`, `internal/dispatch/` | HTTP API, Postgres store, documents, and event outbox |
| Dispatch settings | `cmd/dispatch/settings.go` | the one table of every Dispatch setting envoy-dispatch reads (the libraries it links read `HOME`, libpq's `PG*` and Go's own variables themselves); `envoy-dispatch settings` prints it, and a test fails on any other environment read under `cmd/dispatch` or `internal/dispatch` |
| Migration runners' shared rules | `internal/pgmigrate/` | Dispatch's and the secrets broker's runners: the set loader that refuses a set before anything applies (`Load`), the lock bound on every migration (`LockTimeout`), the watch that names the lock a timed-out migration wanted, and the pre-deploy census of pending migrations (`Census`, and `CensusTables`, its one reading of what a migration locks; the `<version>_<name>.census.sql` a migration declares; `envoy-dispatch census`) |
| GitHub webhook redelivery | `internal/dispatch/redeliver/`, `cmd/dispatch/redeliver.go` | Dispatch's sweep of the App webhook's failed deliveries; `internal/dispatch/githubapp/githubapptest` fakes GitHub's delivery API |
| Document tree (Proof schema) | `internal/dispatch/pmdoc/` | render/parse/diff of Proof documents; fixtures from the fork's headless engine |
| Deploy/runtime         | `deploy/`                                 | compose, rollout scripts, NATS peer setup          |

Every non-inline Proof node has a stable `blockId`. `pmdoc.Parse` mints IDs in document order,
and `EnsureBlockIDs` repairs legacy or duplicate IDs before agent updates are written. Document
settlement is two-phase: it first writes its repairs into the room (`EnsureBlockIDs`, then the
server-owned attributes of each ask block it reconciled) and persists the updates they captured in
the same Postgres transaction as any resulting version and event, then renders and compares
canonical markdown. Each repair is read and written in one Yjs transaction, which holds the
document's lock, so it is computed against the document as it stands: the tree settlement
reconciled was read before its database work, and writing that tree would revert an edit a peer
made since (LEGION-479). Each repair's update is held from the room's own persistence by a
suppression slot of its own (`applySuppressed`; ygo's persistence worker is handed each update on
its own), and every path releases it: a slot nothing finishes holds the worker at the room's next
update. A repair reports whether its transaction wrote anything; one that wrote nothing still
committed that transaction, and ygo hands the worker an update for it too (the document's delete
set), so its slot is finished with that update and the worker takes it rather than storing it. A
settlement that wrote into the room renders its version from the document as it stands after the
repairs (`lockedTreeOf`), so a peer's edit made since its read is in that version too, and credits
that edit's author, whose own settlement then writes no version. It takes the authors with that
tree, so an edit made while the version renders is credited on the version its own settlement
writes, not on this one. A settlement that wrote into the room commits what it wrote even when the
document moved after its read, since the room and its browsers hold it; one that wrote nothing
leaves a moved document to the settlement the move scheduled. A repair is written only into the
document the settlement read (`applySuppressed`): one whose room was evicted and reloaded since is
refused and retried, and one whose room left the server while its transaction committed is given
up and fails the room, its update discarded without waiting for its slot, since ygo then stores it
on the committing goroutine itself (`persistStranded`). The room worker's compaction, except a
failed room's eviction, skips a room whose lock another holder has (`compactIfIdle`), so a
settlement holding the lock does not wait for that worker's exit. Two cases still hang until the
server restarts (LEGION-498): a room that fails while a settlement commits into it, and a second
writer committing into a room while it retires under a repair's commit.
`envoy-dispatch backfill-block-ids` runs the same stamp through `applySuppressed` across every
document. Every write path that changes a document queues that closer once its transaction commits: a live edit (`POST /api/v1/artifacts/{id}/edits`), an uploaded document version (`POST /api/v1/issues/{key}/artifacts`, `POST /api/v1/projects/{key}/artifacts`), and a spec seeded at issue creation - so ask blocks written by any of them become asks without waiting for a later live change. The closer attributes the asks it indexes to the room's most recent mutating actor (`roomState.lastActor`, set by every edit, replacement and seed) when no pending author remains - an edit's own version write has already consumed `pending` by the time settlement runs. A free-text ask block (no bullet list) carries `options: []` on the wire, never JSON null.

The closer's timer lives in memory, so the database says which documents still owe it: every
durable document update writes the document's `doc_settlements_pending` row in its own transaction
(`markSettlementPending`, migration 0063), and the settlement that has read every update deletes it
in the transaction that commits its writes. A settlement that did not commit - one a shutdown's
budget cut short or a room failure dropped - is armed again from that row two ways: a room's load
arms one when the row is there (`onLoadDocument`, unless the load is a settlement's own warm-up),
and `cmd/dispatch` runs `docs.Service.RunSettlementResumption`, which at start and every minute
arms the settlement of each document whose row is a minute old and whose issue is open
(`resumeOwedSettlements`), so a document nobody opens settles too. It runs on an interval because a
rolling deploy stops the old task after the new one has started. A closed issue's rooms arm none
until it reopens. `docs.Service.Shutdown` runs the settlement of each loaded room whose document has
that row, and no other, inside its 5 s drain budget (`shutdownDrainBudget`, within the caller's
deadline: `cmd/dispatch` gives HTTP shutdown and document shutdown one 5 s context between them,
`shutdownTimout`); a settled document's repeat would spend the budget for nothing. It cancels the
database work of any settlement the budget cuts short so its transaction rolls back, and logs for
each document that owed one whether it settled or was left to resume (`dispatch: document settled
before shutdown`, `dispatch: document settlement left to resume after shutdown` with
`shutdown_budget_ended`). A settlement cut short is not an error; a caller's deadline
that passes before Shutdown can read that back is, and its error names the documents that owed
one. A 1 MiB `a_b*` document's settlement took 4.5-6.8 s at load 90-170 on the development
machine, past that budget.

A write never puts one block id on two blocks. `EnsureBlockIDs` keeps a repeated id for the first
holder in document order, and ask rows and anchors are keyed on block ids, so a block written ahead
of an answered ask under its id would take the ask's row and answer, and the question would come
back as a fresh open ask. Markdown a caller writes (a spec seeded at issue creation, an uploaded
document or version, an edit's `insert`, an accepted suggestion's `replace_with`) is refused, naming
the id, when it names an id twice or names one the document holds outside the text it replaces:
`pmdoc.RepeatedBlockID` compares the document the write would store with the live one and refuses
an id the write names that the stored one carries on more blocks than the live one does. It runs
when the markdown is parsed (`pmdoc.ParseForWrite`, before the parse's own id repair, which is the
only place a fragment naming one id twice shows), and again on the spliced tree for an insert or an
accept. An `insert` is `400 INVALID_OP` on `markdown`, every other write `400 INVALID_MARKDOWN`.
A block rewritten in place under its own id is one block, and neither the halves of a block a
splice splits nor a repeat the live document already carries (a browser write can leave one until
settlement repairs it) refuse anything. Only a typed block's markdown can name its id.

Markdown a caller writes is stored as its rendering, so a write whose rendering reads back as
another document is refused, naming what reads back (`pmdoc.RefuseMisreadDocument` for a whole
document, `pmdoc.RefuseMisreadWrite` over the accept path's `pmdoc.NewMisread` for an insert):
`ParseForWrite` reads back the whole document a spec, an upload or a version writes
(`400 INVALID_MARKDOWN`). An `insert` takes one of two paths. A fragment of only table rows
anchored in a table's row goes to `pmdoc.InsertTableRows` (`docs/edits.go:859`), which the route
tries first; the table-row paragraph under the reading rules below says which fragments are rows.
It returns before any read-back, so a table-row insert is not read back, and it can leave a live
document its own markdown reads back otherwise: a row inserted under an aligned column is stored
without alignment and reads back with the column's, and a row whose cells the upload path refuses
for reading back otherwise (a fused emphasis run, a link inside a link) is stored as it renders.
Every other insert is spliced and read back on the document it leaves against the one it started
from, as an accept is (`400 INVALID_OP` on `markdown`), but with the tables' colspans and rowspans
unwritten: it runs once for each insert of a batch, and an insert, written between document-level
blocks, changes no table, so each span reads back the same on both. The reading rules refuse every shape they
know first, so this refusal names one none of them reads, and each is logged (`pmdoc: refused a
write whose markdown reads back otherwise`). Wherever a check reads a write back - here, and in an
accept's, a replace's and an ask edit's checks - only a refusal (`pmdoc.ErrSchema`) is a verdict;
any other error, a panic (`pmdoc.ErrPanic`) among them, is `pmdoc`'s own and answers `500`. A tree
a browser edit makes is not checked, so its rendering can still fail to read back when it is
uploaded again.

Each `doc_updates` row records `content_changed` - whether the update changed the document's
rendered markdown, the only document content a version stores (`pmdoc.Render` of the tree before and
after; the one measure the room's update observer, `updateChangesMarkdown`, a transactional live
write in `applyLive` and settlement's own closure, `closureChangedMarkdown`, all apply) - and
settlement writes a version only when a content-class row lies past the latest version's
`doc_update_version` cursor and the settled document renders differently from that version.
Indexing an ask block fails the second half: it writes an `asks` row and an `ask.opened` event
over words the edit that wrote the block already versioned, and a settlement that versioned the
document for that event alone would write a byte-identical version credited to nobody, staling an
approval pinned to what an agent had just written and closing Legion's design gate with nothing in
the event stream to explain it (LEGION-273). Settlement's tree writes - stamping block ids,
restoring an ask block's server-owned attributes - move the stored Proof state and usually render
the same markdown, so a version for one of those would repeat the version before it too
(LEGION-229 requirement 2); their `doc_updates` row records what they rendered rather than `true`,
so a repair that changed no text leaves nothing past the cursor for a later settlement to version.
The version a settlement that writes none leaves the document at is the one it found, and that is
the number its `block.repaired`, `block.invalid` and retraction events name
(`settlementReconciliation.nameVersion`).
An update that changes only what no rendering carries therefore versions no document by that route:
a margin projection, the attributes a reader's browser editor derives on its own - each heading's
`id` when it opens the document, and each ordered list item's `label` and `listType` on the first
keyboard caret move or edit - or a comment's, suggestion's or ask's anchor mark, wherever it splits
the text it covers. Routes that write a version on request do so whatever changed, so each can write
a version whose markdown equals the previous one's and stale an approval pinned to it: an upload to
an existing document (`POST /api/v1/issues/{key}/artifacts`,
`POST /api/v1/projects/{key}/artifacts`) always inserts one; `POST /artifacts/{id}/versions` and
accepting a suggestion always name one through `NamedVersion`; and `POST /edits` names one through
`NamedVersion` when it is sent with a `summary` and its `changed` is true. That `changed` comes from
`nodeToken`, which counts marks, so an edit that only drops an anchor qualifies. Without a `summary`
that edit writes nothing, because `SnapshotVersion` compares renderings. LEGION-260 is open to make
the edit route write a version only when the rendered markdown changed, keeping `changed` as what it
reports to the agent, with the upload route weighed by the same rule.
`pmdoc.Render` renders a document without its anchor marks, so an escape is decided over a whole run
and `snake_case` never becomes `snake\_case` because a mark starts inside it; an anchored comment's
or ask's `SnapshotVersion` compares that same rendering with the latest version's markdown, so
marking a quote writes no version either - except at most once per document whose latest version an
earlier server stored with markdown this server would render differently - an escape an anchor had
split, whether one it did not need (`user\_id`) or one it was missing (`see [x](y)`, which parses
back as a link), and, since #1326, a block marker in a paragraph line that now takes an escape
(`## not a heading` renders `\## not a heading`), with or without an anchor in the document. The
next snapshot of such a document writes today's rendering of that text, and when the stored version
was approved it leaves the approval stale until a human approves the new one. A browser is credited
as a version's author only for browser edits made while it is connected (`creditContentChange`),
another browser's as well as its own, since a browser edit cannot be pinned on one connected peer;
opening a document or an agent's edit credits it nothing, so a reader whose editor changes nothing
the rendering carries causes no version and cannot stale an approval.
A handler joins its document operations to its transaction with `Docs.Join`, which returns the
transaction's ledger (`docs/ledger.go`), the only way to give a document operation a transaction:
every document write takes its transaction from the ledger and refuses a context that was not
joined (`errUnjoined`): `SeedText`, a rebuild (`RebuildDocument`), the version writes, and every
write that runs through `applyLive` (`docs/mutation.go`) - an upload's replacement, an edit batch,
an accepted or rejected suggestion, an ask's edited text and its answer or resolution, a comment's
anchor mark and its margin record - each of which `applyLive` also weighs by what it leaves
(`docs/growth.go`). An answer the document has no room for is left out of its block, which records
who answered and when, and kept on its ask. The server writes outside `applyLive` are a rebuild,
settlement's repairs, the block-id backfill and the sweep of unrecorded marks. Of those, a rebuild
and settlement's repairs carry caller text. A rebuild's supplied markdown is weighed as a new
document's rendering is (`SeedText`), and its latest version, the server's own text, is restored
whatever it weighs. Settlement's is the answer an ask keeps, written back into a block that returns
to the document: settlement weighs each such answer and leaves out the ones the document has no
room for (`withholdAnswers`). A joined
operation never writes the room: it runs on the transaction's fork of the room's document
(`docs/livewrite.go`), appends its update inside the transaction, and reads through the same fork.
The handler ends the transaction with
`ledger.Commit`, which commits, credits the writes' actor to their rooms, releases the authors a
version the transaction wrote named, then applies and broadcasts the updates, and last publishes
the events its document operations appended, ahead of the handler's own; it defers
`ledger.Discard`, so a transaction that does not commit leaves the room, every connected browser,
every version and the durable document as they were.

While a transaction's write to a document is open it holds that room's writer slot, so another
transaction's joined operation on the document waits for it to be published or discarded, and it
holds off the room's settlement, which runs once the write is published or discarded. The docs
layer takes a document's locks in one order, wherever a handler starts: the owner row
(`lockArtifactOwner`), then the writer slot, then the advisory lock; a joined read takes the owner
row before it waits for the slot. The slot is in memory, where Postgres cannot see a wait for it, so
no transaction may wait for it while holding a lock its holder still needs. A failed room's eviction
flushes and compacts under the advisory lock, so no transaction waits for a failed room to recover
either: a document operation inside a transaction (a handler's, or settlement's own) that meets one
fails with `ErrServiceUnavailable` (`503 DOC_SERVICE_UNAVAILABLE`, whatever failed the room), the
transaction rolls back, and the caller retries once the room has reloaded; so does a write whose
room fails before its first append, since the reloaded room may lack it. A room's own load never
waits for that recovery either - the eviction waits in ygo's `CloseRoom` for the load's ready
barrier, so the two would hold each other - and refuses instead, which ends the eviction; the
replacement room's load then runs the settlement the failure dropped, which the document's
`doc_settlements_pending` row still names.

Successful Dispatch writes on an issue may return top-level `advice` with the issue status, the
count of session-authored messages/comments/asks since the last human event, and the calling
session's two oldest open asks; issue creation and Markdown artifact uploads also report the
document's `decision_blocks` count and, when it holds any, its `unparsed_openers`
(`{count, examples}`): typed block openings (`:::name{`) in its text outside code, written inside a
line or escaped, each example quoting the opening with a little of the text before it. A writer who
meant a block learns it is text; a count of zero alone reads as a document needing no decision
(LEGION-416). Issue-state advice is computed in the write transaction after its event is appended,
so it includes that write. Decision blocks and unparsed openers are read from one parse of the
canonical Markdown after the write transaction commits (`readDocumentBlocks`). Advice queries run
behind a savepoint with a 500 ms
timeout that is restored before the savepoint is released: one failure is logged and omits
`advice` without preventing the write from committing. The consecutive-write query materializes
the last-human fence so its `max(id)` runs once, and relies on the `events (issue_key, seq)` unique
index's issue prefix and commit-ordered event ids; the open-ask query relies on the
`asks_open (issue_key) where state = 'open'` partial index.

Quote-anchored asks and comments retain their inline mark and quote cache, plus the stable `block_id`
of the lowest block containing the complete quote. A quote that spans top-level siblings stays
unpinned. `GET /api/v1/artifacts/{id}/blocks` returns each block's canonical markdown range,
SHA-256 token over its complete Proof state (including inline marks), and `{comments, asks}`
reference counts; `GET .../text` returns the full-document Proof-state token and the document's
latest version number, or `null` when it has none (the live markdown beside it and that version
are two unsynchronised reads, in both directions; `token` is the concurrency primitive). The server resolves
the block when it creates a quote or browser-mark anchor; `envoy-dispatch backfill-anchor-blocks`
fills legacy anchors only when their cached quote has one current match.
`GET /api/v1/artifacts/{id}/blocks/{block_id}` places any one block (`pmdoc.BlockPathOf`, over
the document `readDocument` serves): its path of `{type, id, index}` from the top-level block
down, and for a table block, row or cell a `table` naming the table's id, the row's child index
(0 is the header row), the cell's child index in its row (the indexes `delete_row` and
`delete_column` take, so a spanning cell counts once), the text of the header cell drawn above
the cell and the row's cells as their opening words. The header is found where the renderer
writes the cell (`tableGrid`, laid out on a span budget of its own through the anchored row), so
in a table with colspans or rowspans it is the column the cell is drawn in, not the header row's
child at the cell's index. `GET /api/v1/comments/{id}` and `GET /api/v1/asks/{id}` attach the
same answer for the anchor's `block_id` as `anchor_block` (`api.anchorBlock`), computed at read
time and never stored or carried on lists and events; a block the live document no longer holds
leaves it absent while the anchor keeps its stale `block_id`. The position is one derived field
of those reads, so a document they cannot read does not fail them: the read answers 200 without
`anchor_block` and with `anchor_block_error`, logged at WARN. `api.documentErrorCode` names that
error for the read and for `writeHandlerError` alike, and both take its codes in one order, so
`anchor_block_error` is the code the block route answers the same error with.
`DOC_SERVICE_UNAVAILABLE` is a room or store that could not serve the document, taken before any
cause the error carries: a failed room carries the error another operation failed it with
(settlement's schema refusal, a settlement that failed three times - its warm-up refused because
the issue had closed, among others - a writer's failed or cancelled commit, a failed store write
or load, a history that did not decode), which says nothing of this request.
`DOCUMENT_UNLOADABLE` (409) is a stored history this request itself could not decode
(`docs.ErrDocumentUnloadable`), the one state `POST /api/v1/artifacts/{id}/rebuild` repairs and the
one code the dashboard offers that rebuild for. `DOC_SCHEMA` is a tree outside the schema: one the
document holds is a `docs.OutsideSchemaError`, which `treeOf` and `documentMarkdown` classify where
they read or render a document's tree, so every route that starts from it - `GET /text`,
`GET /blocks`, `POST /edits`, `POST /versions` - answers 409 with its one message, while a schema
refusal of a tree a write itself produced is a 500 (`producedSchemaError`). Both are taken after
the refusals that name the caller's own input or a missing block, so an ask block the renderer
refused stays `400 INVALID_ASK_BLOCK`. Anything else is `INTERNAL`, as `writeHandlerError` answers
it. Only a request that has gone away fails, decided by that request's
own context rather than the error, since a room a writer's cancelled commit failed carries that
writer's `context.Canceled` in its cause. Nor does that read wait for a failed room's recovery
(`docs.WithoutRecoveryWait`): it is `DOC_SERVICE_UNAVAILABLE` at once, where `GET /text`,
`GET /blocks` and the block route wait.

A read of a resident room reads a copy taken under its document lock (`snapshotDocument`,
`crdt.EncodeStateAsUpdateV1`), never the live tree: `GET /text` and the document websocket's
admission check (`loadDocument`), a version's capture (`captureLiveTextAndAuthors`), a read
outside any transaction (`docView`), and settlement's reads of the room (`settleRoomWithin`'s first
read and its version's, and the block-id backfill's read, through `lockedTreeOf`), so a torn read is
never versioned as the document; a repair reads the tree inside the transaction that writes it
(`rewriteLive`), and the unrecorded-mark sweep (`sweepUnrecordedMarks`) inside the transaction that
unmarks it. A walk of the live tree takes no lock (reearth/ygo v1.49.5, `crdt/yxml.go`) while every
peer update and service write holds that lock as it applies, so the walk can read a write halfway
through as a tree outside the schema and answer a healthy document 409 with the repair. A version's
capture holds the room's state lock across the copy and the authors it captures, so an author the
update observer credits is captured only with that update's text; the order - state lock, then
document lock - is never reversed, since only a Yjs transaction's own function holds a document's
lock and none takes a room's state lock. A read that may load its room (a version's capture,
`docView`, `VerifyMark`'s subscription) takes what it reads inside the `Server.Apply` that loads
and holds the room: a room looked up again with `GetDoc` once that Apply returned can have been
evicted in between.

Every decode of a document's whole state takes a pending queue as long as the most items one
update can carry (`newDocumentCopy`, `maxUpdateItems`): the copy, a write's fork (`forkLive`), and
every decode of the stored history - a read with no room resident (`loadDocument`), the history
check behind a room's load and a rebuild's refusal (`validateUpdate`), and the room's own load
(`Server.MaxPendingItems`, set in `New`) - and so does the store's check of each update it appends
(`appendUpdate`, `AppendUpdateTx`), which decodes the update alone. ygo's decoder defers an item
whose parent it cannot place yet - a container a later client's group holds, one outside the
update it decodes, or one garbage collection emptied when a peer deleted it - and parks every later
item of that client behind it as a clock gap, refusing the update once 100,000 are parked, its
default (LEGION-502). A room whose peer deleted a chain of 200,000 nested blocks, or whose
lower-numbered client wrote 100,000 items after one such deferral, then failed every copy with
`crdt: invalid update` while the room itself served it
(`TestDeletingADeeplyNestedLiveTreeNeedsNoStackPerLevel`,
`TestACopyHoldsEveryItemItsRoomParksForOneClient`). Once that room was evicted or the server
restarted, the same document read as `409 DOCUMENT_UNLOADABLE`, its room did not open, and the
rebuild that code offers, whose history check refused it too, replaced its history with its latest
saved version (`TestAStoredHistoryLoadsWhatItsRoomParksForOneClient`). A browser update of more
than 100,000 items written against blocks the document already holds, such as 75,000 paragraphs
with their block ids written ahead of one, failed the store's check, so the room failed and dropped
the edit (`TestTheStoreTakesOneBrowserUpdateItsRoomTook`). ygo refuses any update that declares
more than `maxUpdateItems` items, so no decode of one parks past that queue; in a room, which keeps
what it parks across updates, it is also the most the room's peers can park, about ten times ygo's
default. One check still decodes one update alone at ygo's default: ygo's, of an update the
service broadcasts (`Server.BroadcastUpdate`). It refuses an update of more than 100,000 items that
lean on items outside it, such as a settlement that stamps that many blocks' ids, and the room
fails.

A room whose last peer leaves stays resident until it has been idle for a minute
(`roomIdleTimeout`), when ygo's idle sweeper evicts it. ygo's default, eager eviction, evicts a room
the moment its last peer leaves even while a `Server.Apply` is inside its callback on it (reearth/ygo
v1.49.5, `provider/websocket/peer.go` checks only the peers): the callback's write then lands on the
evicted room and reaches the store only through its retiring persistence worker, while the next
access has already loaded the store without it and serves, and takes, the next write on a document
missing the first. Two such writes, each a diff of the same document, merge into a document neither
wrote, and into one holding no block at all once each kept a block the other replaced: the
healthy-room probe met it as a socket closed with `DOC_SCHEMA`. The idle sweeper evicts only a room
no Apply holds or has touched since its last peer left (`provider/websocket/idle_sweep.go`), so every
write a room the sweeper evicts has taken is durable before another instance of it loads, and a peer
that returns within the minute rejoins the warm room. `TestAHealthyRoomUnderWritesIsNeverRefused`
checks every load of a room against the writes its earlier instances took. ygo's `CloseRoom` checks
only the peers as well, and the service still calls it to close an issue's rooms (`SetIssueClosed`),
for a room with an editor at `Shutdown`, and to evict one (`evictRoom`): a write that commits on a
room it has retired reaches the store through ygo's stranded persistence, on the committing goroutine
(`provider/websocket/persistence.go`). A committed write's publish (`publishLiveUpdate`) is already
durable and suppresses that append; the room's own update observer finishes its suppression slot
before ygo's persistence observer runs (`onLoadDocument`), so the stranded append never waits on the
publish it runs inside, and the publish, its request and the document's writer slot are released
(`TestAPublishSurvivesItsRoomsWorkerRetiringUnderIt`, `TestAWriteSurvivesItsIssueClosingAsItPublishes`).
A publish whose room `CloseRoom` removed has no peer left to broadcast to and returns.

The room's update observer (`updateChangesMarkdown`) renders a replica of the room, not the live
tree, since ygo fires it after the transaction has released the document's lock and another write
can be integrating meanwhile (`renderedReplica`). The replica is copied from the room on its first
update and then brought up to date under the room's lock with what the room gained since its state
vector, as `forkLive` brings a fork up to date. The update the observer is handed cannot stand in:
two transactions' observers run concurrently in either order, and each update carries the room's
whole delete set. On a 1 MiB document a catch-up after one typed character took about 8 ms where the
render took about 160 ms and a whole copy about 420 ms, and the replica holds about 80 MiB of heap
while its room is resident.

Document edits (`POST /api/v1/artifacts/{id}/edits`, `docs/edits.go` `applyOperation`) are
`replace`, `delete`, `insert`, `retype`, `move`, `delete_row`, and `delete_column`. Inside a code
block a `replace`, like an accepted suggestion, writes `with` as the code's literal text
(`codeReplacement`), and none of the rules below apply. Markdown cannot carry line breaks at the
end of the code's text, which read back without them, and an accepted suggestion writes its code
as it reads back (`acceptedCode`): where only line breaks follow it, its text loses the line breaks
that end it. A line holding only spaces and tabs is written as sent: a list item takes no more
than its own columns from a blank line, as the browser editor's parser does (`listItemColumns`),
so both readers keep what the line holds past them. A footnote definition takes none, and a blank
code line inside one is written without the definition's indentation (`writeCodeLinePrefix`). A
suggestion can run past the code into the blocks after it. One that takes all of the text of the
textblock it ends in leaves nothing after its own text, as at the code's end, so the same rule
applies. One that ends inside that text is written as sent, and the rest of that text joins the
code after it. Code on other lines stays as it was. An accept whose code changes how a block
around it reads back is refused, advising rejecting the suggestion
(`refuseAcceptedCodeThatReshapes`). It names the typed block holding the code when the
document-level block that reads back otherwise is the one holding the code, and otherwise names
that block, such as the list of a task item the suggestion empties ahead of its nested list. The
edit route's refusal of the same shape advises moving the code out of the typed block instead,
and a reject in code, like any reject, is not read back. A line of colons in code inside a typed
block is kept: the browser editor's
parser ends a typed block at a line of at least its fence's colons, with spaces and tabs around
them, starting less than four columns
past where the typed block's own lines start on the written line, even inside fenced code -
counting the width of the list markers and `> ` around the code, advancing a tab to the next
multiple of four from the column it stands at, and finding nothing closing in a blockquote inside
the typed block - so the renderer writes the typed block's fence longer than every such line
(below). The engine's reading of each shape is `pmdoc/testdata/typed-fence-lines.json`, which the
fixture generator writes for a callout at the top and inside a blockquote, list items, a footnote
definition and another callout. Text a `replace` writes that would read as block syntax at a line
start is written escaped, so it reads back as the characters: `---`, `***`, `~~~` or `::::` over a
paragraph is stored `\---` and so on (the renderer's line-start escapes). Beside an emptied
paragraph it is written the same way, since an empty paragraph is not written. Accepting a
suggestion (`POST /api/v1/comments/{id}/accept`, `docs/marks.go` `applySuggestion`, its checks in
`docs/accept.go`) writes blocks, so it stores what reads back as the live document, and refuses
what cannot, naming `replace_with`. A table the splice cut is padded to its width as the browser
editor's table plugin pads it (`padCutTables`, `pmdoc.PadTables`), where the browser's accept
writes the replacement where `Splice` does (`padsLikeTheBrowser`): inline text, code's literal
text, an empty replacement (which deletes the matched text, by this accept's own rule, where the
browser's accept of an empty suggestion only clears its mark), or block content over exactly the
two textblocks the browser's accept replaces whole (`pmdoc.MultiblockRange`, which reads each
textblock one position short of its end as the editor does), the first of them a document-level
block. Other block content over a table, such as a list over one cell's whole text, or one
running from a paragraph in a callout, a quote or a list item, or from one character into a
paragraph or heading, is not padded. Nor is anything running from one table into the next, or
from one body row into another (`joinsTwo`), which the browser can join into one table or row,
even where the browser's result would read as the padded one would; the reject refuses a join of two tables the same way. A range from the
header row into the first body row is padded, since those rows cannot join. An accept that is not
padded is judged as the splice left it, and the checks below refuse one that cut a table. Block
content the browser takes whose range ends short of the last textblock's end, within the editor's
tolerance (`- a` over `abc Next` with the cell holding `Next e`), keeps the rest of that textblock
(` e`), which the browser's accept drops with the textblock; the cell keeps text, so nothing there
is cut or padded. `pmdoc/multiblock_test.go`'s table is all that pins `pmdoc.MultiblockRange` to
the editor, so a proof-sdk pin bump that changes `resolveStructuralMultiblockRange` re-checks it.
It then settles the blocks it changed (`settleAccepted`,
`pmdoc.AgreeWithReadBack`): the empty halves a block replacement leaves of the textblock it lands
in, which carry no block id, go where the renderer does not write them, and each list and list
item takes the spread its markdown reads back with, paired as far down as the read-back check
pairs blocks, so two paragraphs in a tight list item leave the item spread, in a list that already
reads back otherwise too, a block over an item's text leaves no empty line before its nested list,
and an item beside them in a list they make loose is spread as its markdown now reads. One the
accept left unchanged whose spread already read back otherwise before it, such as a spread its
markdown never carried, keeps that spread. Every other block, mark and node stays as it was. An empty replacement keeps the paragraph it empties, which is not written beside other
blocks. Outside an ask it then runs, over every document-level block it changed,
`refuseUnreadableReplacement`'s check (`refuseUnreadableAccept`) and the shape comparison
(`refuseReshapedAccept`: `pmdoc.BlockShapeError`). Both write a table without the empty cells its
colspans and rowspans add, which the read-back of the whole document below writes, so an accept
across many tables costs no more there than the cells those tables hold. A non-empty replacement
inside a typed block is
checked by that block's own `Splice` content rule, and `refuseBrokenAsks` checks an ask's
`paragraph+ bullet_list?` rule. Last, the whole document is read back (`refuseMisreadAccept`:
`pmdoc.NewMisread`), each document-level block beside the ones around it and with its attributes
and text, a column without alignment expected back unaligned as the renderer writes it: an accept is
refused where a block now reads back otherwise that did not before, such as a task item emptied to
`- [ ]`, which reads back as a plain item. Each block that reads back otherwise is found as far
down as its markdown still pairs, and one that already read back otherwise the same way before,
under the same block id (a paragraph's text differing in the same characters, an attribute with
the same values, a block pairing with nothing), is not the accept's, even inside the block it
changes: text beside a task item already read back as plain, or in a paragraph ending in a hard
break that reads back with a literal backslash, is stored, while a second task item emptied there
is refused. Nothing else in the document switches the check off, except a document the parser
already refuses, where the block checks alone judge the accept. That refusal names what reads
back and advises rejecting. A
list an accept writes beside a list of its kind is written with the other marker (below), so the
two read back as the two lists it made. An accept parses its text as blocks
written into the document (`pmdoc.ParseFragment`: a leading `---` is a rule, as `***` is, except
that a closed front-matter block is front matter where the text lands at the document's start,
at the start of a top-level first block's text), so a rule or a list over a whole paragraph is
written as that block and kept, while one that leaves a block the document cannot read back,
such as an empty callout in a list item, is refused: the document
stays as it was and the suggestion stays open. The person accepting cannot change the text, so
the refusal (`acceptRefusal`) says what the text writes where it lands - for a same-id rewrite of
a typed block, in the block that typed block stands in - and names what they can
do (reject the suggestion, or reply asking for text the block can hold), never an edit-route
operation; it says the text empties a paragraph only when the text renders no content, and then
offers deleting the paragraph only where the rest of its block stands without it, and the whole
block only where the paragraph is all it holds. A reject is not checked, since it gives back the
text the insert started from. Everywhere else `replace` is
inline: `with` parses through `pmdoc.ParseInline` (paragraph-only block grammar), so a multi-paragraph
`with` is `INVALID_OP`, so is any non-empty `with` that renders to no inline content (a line
indented four spaces or a tab, which markdown reads as a code block, or whitespace alone — an
empty `with` deletes the match on purpose, and a container left holding only the emptied paragraph
reads back holding it; refusing the rest is LEGION-280, since
splicing nothing over the match silently deleted the caller's text), and a leading marker of a
*different* kind from the matched block's own is
literal escaped text. A `with` opening with a marker of the *same* kind as that block's own would
render it twice and is `INVALID_OP` on `with` (`replacementMarkdown`), naming the marker the block
renders and, in backticks the caller can paste back, their own text with the marker's significant
character escaped; the exception is a heading
rename whose `find` carried a heading marker, where the repeated marker is dropped, and `with` may
change the level only when `find` named the block's actual level — `# ` is the level-blind
selector, so a generic `find` renames the text and keeps the level. A hard line break inside `with`
— two trailing spaces or a backslash before the newline — puts what follows it at a true line
start, where a heading, bullet, `1.`/`1)` ordered or `>` blockquote marker is `INVALID_OP` on
`with` as well (`blockMarkerAfterHardBreak`), since replace is inline and that marker can only be
written as escaped literal text continuing the matched block, never as the block it names. A hard
break is itself `INVALID_OP` when the matched textblock is a heading or a table cell
(`pmdoc.TextblockAt.OneLine`), which are written on one line, so the break would end the block; a
line break inside a code span or inline HTML there ends it too, and the replace is refused because
the block would read back as blocks of another shape (`refuseReshapedReplacement`). A bare
newline is a soft break, which renders as a space and reaches no line start; an ordered marker
interrupts a paragraph only numbered a lone `1`, as the browser editor's parser reads it, so `1.`
and `1)` are refused, while `01.`, `001)`, `02.` and `10.` are not; and marked text opens with its
mark's delimiter, not the marker — none of the three is refused. A batch that leaves the document's
semantic identity unchanged — `nodeToken` over the whole tree, inline marks included — mints no
version, named or not, and the response carries `changed: false` with `unchanged_ops` naming each
operation that changed nothing (`docs.EditOutcome`). Every refusal names its operation index and
its anchor: `docs` dresses quote, heading-anchor and cross-block misses (`ErrQuoteNotFound`,
`ErrAnchorAmbiguous`, `ErrQuoteSpansBlocks`), and `isEditRefusal` keeps the service's internal
prose off them. `delete` takes `find` or
`block`; a `find` covering a textblock's whole text removes that block
(`pmdoc.DeleteTextblock`: it also drops a list, list item, or blockquote it empties, hoists a nested
list into the place of a bullet whose text goes, and refuses a bullet with other content with
`ErrListItemContent` naming `delete {block:"<item id>"}`; `pmdoc.DeleteBlock` serves `block` and
reports any emptied container's content rule as `INVALID_OP`; both leave an emptied footnote
definition holding one empty paragraph, which both parsers read back as the definition, so its
reference stays a reference). `move` relocates the block with
`block` to the document-level boundary of an insert anchor (`pmdoc.MoveBlock`); insert and move
anchors are a quote, `start`, `end`, `heading:<title>`, or `block:<id>`. An insert's `markdown` is
read as text written into the document (`pmdoc.ParseFragment`), so a leading `---` line is a rule,
as `***` is, except that a closed front-matter block is front matter where the insert lands at the
document's start (`start`, or before the first block); the accept and the insert decide that with
one rule (`docs.opensDocument`). A document with nothing in it holds one empty paragraph
(`pmdoc.EmptyDocument`: an issue created without a spec, or a document whose last block was
deleted), and an insert into it takes that paragraph's place wherever it is anchored, rather than
leaving an empty line beside what it writes; it therefore lands at the document's start, and reads
front matter there whatever its anchor.

A write runs on its transaction's fork of the room, so a browser change made while it is in flight
merges with it rather than blocking it, and the merge can annihilate the write: `pmdoc.Update`
splices a paragraph's text in place, a browser's paragraph delete is an element delete, and ygo's
delete cascades over the element's children, so whichever side merges first the inserted run ends
up tombstoned or live under a tombstoned element. The document outcome is right - the human's
deletion wins - but a write that committed, versioned and answered 200 anyway would tell the agent
its edit applied (LEGION-269). Each write records what its own Yjs update inserted and where
(`docs/lostedit.go` `lossCheck`, over `pmdoc.AuthoredTextRuns`): per operation, the blocks whose own
inline content it wrote (`pmdoc.BlocksGainingText`, the same `simpleDiff` insertion `pmdoc.Update`
makes, so a block whose text only shrank, an attribute-only change, a `delete` with or without
`find`, `delete_row`, `delete_column` and an unchanged operation all write nothing that a
concurrent change could remove and are never reported lost), and the clocks of the text items the
update left live in each of them. Two windows read it back. Before the version is rendered,
`captureLiveTextAndAuthors` brings the fork up to date with the room and `refuseLostWrite` refuses
the whole batch with `409 EDIT_LOST_TO_CONCURRENT_CHANGE`, naming the lost operations and the
room's other connected participants; the transaction rolls back, so no version, durable update,
room update or broadcast survives, and the caller re-reads and decides again. After the committed
write reaches the room, `recordPublishedLoss` reads it again: nothing can be undone there, so the
response is `200` with `lost_ops` naming the operations whose text the live document does not
carry, `[]` when everything survived, and `null` when no verdict was reached: the publish failed
and the room is reloading, or the room holds a tree past the schema's depth bound, which a re-read
answers with `409 DOC_SCHEMA` and the repair. An operation's text counts as surviving while it is
live inside an element carrying the block id it was written into - any such element, since a browser move can
leave an id on two until `EnsureBlockIDs` repairs it - so a concurrent range delete around the
agent's own insertion, or a keystroke in the same paragraph, is an ordinary success, while a
deleted paragraph, a deleted ancestor and a browser move that strands the run in another block are
all losses. Accepting a suggestion carries the same check under the comment's id instead of an
operation index: a loss in the first window is the same `409` and leaves the suggestion open, and
one in the second answers `200` with `lost: true` (`null` when no verdict was reached, as above).
That path stamps no block ids, because `EnsureBlockIDs` would also repair a repeat the live
document carries, which is settlement's to repair and a write's to leave as it found it.

Accepting a suggestion (`POST /api/v1/comments/{id}/accept`, `docs/marks.go` `applySuggestion`)
splices its `replace_with`, which unlike an edit's `with` may be blocks, with ProseMirror's range
fitting (`pmdoc.Splice`). A non-empty replacement fitted into a typed block stays inside it, and a
fit never replaces the typed block it lands in: a callout takes what its content rule allows, a
code block included (the engine oracle's `callout-paragraph-and-code` case), and an ask takes any
block at this step, since `Validate` lets an ask hold other blocks while a browser edit passes
through. The one exception is a replacement holding exactly one block of the typed block's own
type under its id, at any depth, which is that block rewritten: it replaces the block in place
rather than nesting inside it, and the replacement's other blocks, and any it sits inside (a
blockquote, a list item, a typed block under another id), go in the same parent, where they stand
in the replacement. The same type under another id, or under none, is a new block and lands inside
like any other. Either way the accept keeps only what reads back as it wrote it (above). The accept
then reads each ask by its id before and after the splice (`docs/ask_blocks.go` `askReadability`,
`docs/marks.go` `refuseBrokenAsks`): an ask is unreadable when settlement's parse fails, when its
children break the content rule `paragraph+ bullet_list?` (`pmdoc.AskContentError`: a paragraph
after its options or a second bullet list parses, but the browser editor drops such an ask from the
shared document when it renders it, and settlement then retracts it), or when it repeats an earlier
ask's id. The first ask left unreadable whose id was readable before is `400 INVALID_ASK_BLOCK` with
that reason, so a question given a code block, a paragraph after the options, a second list or an
emptied question is refused. An ask under a held id is refused one step earlier, by the write's
block-id check (`400 INVALID_MARKDOWN`, the block-id paragraph above). An id the document already held unreadable
(a browser edit can leave one, and an upload can carry it on) does not refuse an accept, whether the
accept leaves that ask alone or writes into it, unless the accept adds a second ask under it: an id
that gains an ask is refused whatever it held, since the id repair would hand the held ask's row
and answer to whichever comes first. A reject (`POST /api/v1/comments/{id}/reject`, the same
`applySuggestion`) is never checked, its asks included, since removing the text a browser insert
added gives back the document the insert started from. It deletes that text as the browser editor's
reject does (`rejectedInsert`): the insert's runs that meet across a block boundary, nothing but
the boundary between them, are one range (`pmdoc.MarkSpans`), so the blocks join, which undoes the
split an insert made (Enter typed while suggesting), and a table the range cuts is padded to its
width as the editor's table plugin pads it (`pmdoc.PadTables`, after prosemirror-tables'
`fixTables`). A removal the
document cannot hold, one the schema refuses or the renderer cannot write, is refused, `400
INVALID_OP` on `anchor`, advising accepting the suggestion or editing the document
(`rejectSpliceRefusal`), with the document unchanged and the suggestion open. A reject is not read
back. Where it stores otherwise than the browser's reject, or refuses what the browser stores:
- Text without the insert's mark between two of its runs is kept, each run deleted on its own; the
  browser deletes that text too, and the editor leaves it when someone suggests inside another
  person's insert or pastes into it outside suggestion mode.
- An insert running into an ask or callout from the text before it is refused; the browser drops
  the emptied ask or callout.
- One running from one table into the next is refused; the browser joins the tables.
- One over a header cell and the body cell below it (`| QQ |\n| --- |\n| ZZ |`) is refused; the
  browser stores the emptied cells.
- `# HelloQQ` then `## ZZ world.` keeps two headings (`# Hello`, `## &#32;world.`); the browser's
  delete joins them, and `Splice`, which makes ProseMirror's replace, keeps headings of two levels
  apart.
- A cell at a row's end into the next row's first cell keeps the rows apart and pads the next row,
  whose remaining cells move one column left keeping their own alignment, so each reads back with
  its new column's; the browser joins the rows and widens the table.
- One into a one-column table's only header cell stores one empty header cell; the browser leaves
  the header row empty and adds a row, which the schema cannot hold.
- A padded cell takes its column's alignment, where `fixTables` makes it left, so the column reads
  back as it was.
- A paragraph's end into a footnote definition stores the reference reading back as literal text,
  and a paragraph's end into code turns the code's line break into a soft break.
- Since a reject is not read back, it can store a document that reads back otherwise: a table
  column's alignment, an emptied paragraph beside other blocks (which is not written), a task item
  emptied to `- [ ]` (which reads back as a plain item), and nested lists or footnote blocks.

An accepted replacement no level of the document can hold where the suggestion sits, such as a code block over a table cell's whole text, is `400 INVALID_OP` on
`replace_with` (`pmdoc.ErrReplacementDoesNotFit`), and inline text over a range that runs into an
ask or callout from the text before it, at any depth (inside a blockquote, a list item or another
callout too), is `400 INVALID_OP` on `anchor` (`pmdoc.ErrJoinEmptiesTypedBlock`): ProseMirror's
join would leave that block empty, and the browser editor drops it, which for an ask retracts it. A refused accept writes nothing, and the
suggestion stays open; the dashboard's margin shows the refusal's message and offers no Retry for
`INVALID_ASK_BLOCK`, `INVALID_MARKDOWN` or `INVALID_OP` (`useCommentActionQueue` `actionFailure`),
since the same accept is refused every time.

`delete_row` and `delete_column` each take a table `block` id and a zero-based `index`, and mutate
the table in place. Row `0` is the header; deleting it promotes the first body row into the header,
including its cells' alignment. An index is required. A missing, non-integer, negative, or out-of-range
index is `INVALID_OP` on `index`, naming the supplied value and the table's actual row and column dimensions; no operation
partially mutates a table. Parsing pads a short row to the table's width, as the browser editor's
table plugin does on load: Milkdown's gfm preset installs prosemirror-tables' `tableEditing`, whose
`fixTables` pads a table a transaction brings in (verified on an `EditorState`; that the browser
loads by a transaction is y-prosemirror's sync, not checked in a browser). The headless engine runs
no plugins and keeps the short row. The table's width is its delimiter row's, and padding is
bounded: parsing pads only while the markdown one write sends adds at most 10,000 cells in all -
a spec, an upload or a version; every operation of an edit batch, table-row fragments included; or
an accepted suggestion's replacement together with the tables its splice cuts, as a reject's cut
tables (`pmdoc.PadTables`). A larger write is refused before its cells are allocated
(`pmdoc.ErrTablePadding`: `400 INVALID_MARKDOWN` for a document or an accepted suggestion's own
replacement, `INVALID_OP` on an edit's `markdown`, on an accept's `replace_with` for the tables
its splice cuts, or on a reject's `anchor`), naming the table, the cells it writes, the cells
padding its rows to the table's width would make, and the limit. Every table
goldmark reads is charged, however it is written, a header narrower than its delimiter row among
them, which goldmark pads with the rest: `findTableRows` reads a paragraph's lines as goldmark's
table transformer does, and `TestFindTableRowsReadsWhatGoldmarkReads` holds the two together. A
conditional batch runs its operations to check its anchors and preconditions as well as to apply
them, each run on a budget of its own, so its padding is charged once. A read-back, a check that
parses the rendering a write would store, pads the tree's own short rows, as a rendering leaves
them where spans go unwritten, on a budget of its own for each parse: 100,000 cells
(`maxSpanCells`, as many as one render lets spans add), which a table the browser editor has padded
reaches only when its spans pass what the renderer writes. A write into a document whose read-back
would pad more is refused rather than stored unchecked. A quote is matched by its text, which
padded cells do not hold, so a quote's table is read as markdown only while it needs no padding.
Each padded cell takes its column's alignment, as its rendering
reads back, so column deletion operates on that complete representation and leaves every
non-selected cell intact. Deleting the last remaining body row or any row's last remaining column
is refused, retaining the table block. Table `references` from `GET /api/v1/artifacts/{id}/blocks`
aggregate anchors pinned to descendant cells. A row or column deletion that would remove an open
ask or unresolved comment anchor is `INVALID_OP` on `index`, naming the axis and anchor ids;
answered asks and resolved comments remain historical and do not block it. Block ids stay with
moved, retyped, and table-edited nodes, so the ask reconciliation keeps a moved ask; a removed
block retracts its ask only while the ask is open (an answered or resolved ask is already closed,
and `asks_answer_state_check` forbids a resolved row that still carries an answer). Within one
atomic batch, an operation that names a block cascaded away by an earlier `delete {block}` fails as
`INVALID_OP`, naming the earlier operation and the parent block rather than treating it as an
unknown id. `ApplyOps` stamps `EnsureBlockIDs` on the live tree before resolving operations so
every block is addressable.

`POST /api/v1/artifacts/{id}/edits` accepts an optional precondition choosing exactly one
document token or one-or-more `{id, token}` block tokens. A document token protects every
operation; a block guard must name every content block the resolved batch changes, while unrelated
sections can change concurrently. Insert and move require the document token because their meaning
depends on document order. Tokens include inline marks, so a fresh anchor makes the relevant
document or block guard stale. After resolving the artifact, the conditional path takes a bounded
in-memory gate keyed by its artifact id before starting the write transaction or warming its room,
then takes the document's owner row and writer slot, then
`pg_advisory_xact_lock(hashtext(artifact_id))`, and enters its one Yjs transaction; it
reads, checks, resolves, and applies the batch inside that transaction. Admission waiters hold no
database connection; `EDIT_QUEUE_FULL` is a `429` response that means back off, while
`PRECONDITION_FAILED` means re-read. This protects unrelated document reads, websocket
authorisation, room loading, settlement, and browser persistence from a stale-edit flood exhausting
the shared pool. The advisory → Yjs order matches durable writer and persistence
ordering and prevents both live-writer check-to-apply races and a lock inversion. `AppendUpdateTx`
holds the same transaction-scoped room lock for every durable `doc_updates` append. A failure
returns `409 PRECONDITION_FAILED` with each mismatched block or document token and current tokens,
and commits no update, version, or event. An uncovered block guard returns `400 INVALID_PRECONDITION`
before mutation. This lock orders commits rather than timestamps, so transaction-start `created_at`
values and a different lock cannot admit a stale write. Every edit response carries `token`, the
whole-document token of the tree that edit's own transaction wrote, taken while it still held the
room's writer slot (`docs.EditOutcome.Token`, from `editBatch.outcome`; a batch with no operation
reports the document as it stands). It is never re-read after the commit, where a concurrent
writer's change would fold into it and let the caller's next guarded edit pass without having seen
it. Settlement cannot move it: `tokenAttrs` hashes neither block ids nor server-owned attributes.
So a chain of guarded edits passes each response's token as the next `precondition.document` and
reads the document once.

The room lock and the document's **owner row** — the artifact itself for a project document, its
issue otherwise — are governed by two rules, both load-bearing. **Level:** no lock on `issues`,
`artifacts`, `projects` or `asks` is `for update`. Weaker levels are free — `for no key update`
where a writer must serialise against other writers of the same row, `for share` or `for key share`
where it need not — and `TestNoForUpdateOnOwnerTables` (`api/lock_level_test.go`) enforces the
prohibition over the way these locks are written: a `for update` spelled in a string literal, or
in a chain of literals and package-level constants, `var`s included, resolved to a fixpoint, which
is every site here. An operand the check cannot resolve is read through its own string literals
alone, spliced in with a space at each end, so a clause that forms across that splice is refused
too, and only a clause the scanned text never spells, including a keyword the splice splits
mid-word, is outside its reach and is a review matter. What `for update` costs is the
`for key share` a foreign key takes: under it an insert into `doc_updates`, `doc_snapshots`,
`doc_checkpoints`, `comments`, or any child table added later waits on the owner row, and a
durable writer holding the room lock then deadlocks against whoever holds that row.
`for no key update` conflicts with itself exactly as `for update` did, so writers of one owner
still serialise and the per-owner event sequence is unchanged. One transaction would
still need `for update` on these tables: one that deletes such a row or changes a key column,
which is what the weaker level does not cover. Nothing does either today; the check refuses it if
something starts to, and that red is the prompt to revisit this rule, not to reach for a weaker
level that would not hold. **Order:** a transaction that takes both an owner row and the room lock
takes the owner row first. The durable writers take no owner row at all — `appendUpdate`,
`CaptureSnapshot`, `PruneAfter`, `Compact` and `Delete` reach the room through `withRoomLock`,
which holds a session-level `pg_advisory_lock` on its own connection before it opens a
transaction, so no row lock can precede it there — which is why order alone could not be the
whole rule; but a transaction that does take both, such as an upload appending its event after
writing the document, deadlocks against a settlement if it takes them the other way round.

Each rule holds off a failure. Under `for update` those locks block a child insert, and a durable
writer holding the room lock deadlocks against a settlement or an event append holding the owner
row; on the live path the loser is `failRoom`, which evicts the room and drops the update. At the
weaker level with the order broken, a project-document upload that takes the room lock before its
event's owner lock deadlocks against a settlement holding that row.

**One caller, one connection.** Nothing holding a connection of the shared pool (`store.Pool`)
- a transaction, an open cursor, which holds its connection until it closes, or a connection
taken with `Acquire` - acquires a second one from it, and nothing a connection-holder waits for
needs one either. A transaction holds its connection until it commits, and a caller that asks
for another one while holding a row or advisory lock waits for a connection only the callers
queued behind that lock can release. Two anchored writes and two settlements of one issue's
other documents closed that cycle on a four-connection pool, and only `pg_terminate_backend`
recovered it. Rationing connections does not fix it and neither does a bigger pool: the queue
behind one writer's issue lock is unbounded (a settlement per document, every issue-owned event
append, the architecture importer), so the size only moves the number of callers it takes.

The rule enforces itself. `store.Pool` keeps the pgx pool private and every way it hands out a
connection - `Query`, `QueryRow`, `Exec`, `Acquire`, `AcquireFunc`, `AcquireAllIdle`,
`CopyFrom`, `SendBatch`, `Begin`, `BeginTx` - refuses one taken while the caller already holds
one, with `store.ErrNestedAcquire`, so a second acquisition fails a test instead of wedging
production. Every entry point that opens one of this pool's transactions marks its context with
`store.WithTransactionTracking`: `api.Register` marks every route and the document websocket,
and settlement, the architecture importer's `Sync`, the outbox publisher's `Run`,
`MaterializeAt`, the three CLI backfills (`BackfillBlockIDs`, `BackfillAnchorBlocks`,
`CompactAll`), `rebuild-refs`, the startup seeds and each migration mark their own. There is no
exception: `docs.SetIssueClosed` takes its caller's context so the pool sees it even though it
runs after that caller has committed. A durable append holds its connection directly rather
than through a transaction, so `withRoomLock` marks its context with `store.HoldsConnection`
for as long as it holds that connection and the room's advisory lock, and `Query` marks the
caller for as long as its rows are open, so a cursor counts as the held connection it is. A
refusal logs its stack once per call site, so a caller that trips it in a loop cannot flood the
log; the error itself is returned every time. `store/pool_test.go` and
`api/anchored_write_concurrency_test.go` hold the halves: the refusal on every guarded method,
concurrent anchored writes, and two settlements queued behind one held write on a
four-connection pool.

**Every pool's size is set in code.** `store.Open` fixes `MaxConns` at `store.sharedPoolSize`
(16), so neither the task's CPU allotment nor the DSN another repository's URL builder writes
decides it, and a `DATABASE_URL` that carries `pool_max_conns` is **refused at open** rather
than silently overridden — the parameter is read from `pgx.ParseConfig`'s `RuntimeParams`,
because `pgxpool.ParseConfig` is the layer that folds it into `MaxConns` and deletes it, so
pgxpool's own parse cannot answer the question and a raw-string scan answers a different one.
The other pool parameters are not refused, because the shared pool keeps whatever minimums the
connection string sets. pgx's default, `max(4, NumCPU)`, is four on production's one Fargate
task at `cpu="512"`, and four is a connection per blocked writer: with four writers parked on
one issue's row lock, a read of an unrelated issue waits for as long as the lock is held. The
shared Aurora cluster has thousands of connections spare, so 16 is deliberate headroom.

Work that genuinely needs its own connection while a transaction is open does not take it from
the shared one; it takes it from a pool of its own, opened on demand from a copy of the shared
pool's configuration and closed with it (`store.Pool.separate`). A cold document room loads on
the rooms pool (`store.Pool.Rooms`, four connections): that load is deliberately outside the
writer's transaction, because the room outlives the request and its updates must not roll back
with it. Everything the load needs comes from that pool, `onLoadDocument`'s issue read included
- a writer holding a connection and the issue's row lock waits for the load, so a load that
waited for the shared pool would close the same cycle without a transaction of its own. The
pool belongs to the store rather than to each `PgVersioned`, so a caller that constructs one to
read a document borrows those connections instead of opening more. The load balancer's probe
takes the other one: `store.Pool.Healthy` reads the highest applied schema version from the
health pool (one connection, used by nothing else) under a `store.healthProbeTimeout` deadline
derived from the caller's context, two seconds covering dial and query, and `/healthz` answers
with the result, reporting that version as `schema_version`. That constant's
doc owns the argument for the bound: every prober reads silence as a dead process, the
tightest of them allows three seconds (`deploy/compose/dispatch.compose.yml`,
`deploy/scripts/autodeploy.sh`) against the ALB's five, and nothing else bounds the wait
usefully — a cold dial is floored at two minutes by `pgxpool`, forty times that tightest
deadline, and once the connection is up nothing bounds the query at all, because a request
context carries no deadline. Warm is production's normal state, so the unbounded case is the
one it runs. Both separate pools zero `MinConns` and `MinIdleConns`, so a floor meant for the
shared pool cannot become a target a one- or four-connection pool can never reach, and both
are deliberately unguarded — a load or a probe taken under an open transaction must be served,
not refused, and a separate pool cannot close the cycle the guard prevents.

The migration runner takes one more connection outside the shared pool. `pgmigrate.Exec`, which
applies every migration, first sets the transaction's `lock_timeout` to `pgmigrate.LockTimeout`
(five seconds), after the runner's advisory lock, so a migration queued behind a long transaction
fails the boot rather than holding every read and write of its table behind its request. While
the migration runs, its lock watch reads `pg_locks` for the transaction's backend every 200 ms on
a connection it dials from that backend's own configuration (`pgx.Conn.Config`) at its first
reading and closes when the migration ends, so a migration faster than one reading dials nothing.
The watch's last reading is how the failure names the lock and its holders: Postgres's own error
says only `canceling statement due to lock timeout`.

Everywhere else a read runs through the transaction it is already inside: the issue state an
anchored write checks before it stamps its mark (`issueOpen` through `queryFrom`), table anchor
checks (`rejectLiveTableAnchors`, `rejectUnindexedTableMarks`), recorded mark refs, and the
suggestion kind and browser-mark verification the comment routes ask for while their
transaction is open. Settlement marks its injections owner-verified (`withOwnerVerified`)
because it has already read and locked the owner row in its own transaction, so `allowInject`
does not read it again. An injection that is not owner-verified does read the issue through the
shared pool, and that read is outside the cycle only because ygo runs `OnInject` before
`getOrCreateRoom` (`provider/websocket/inject.go:311-320`): an injection refused there has
published no room placeholder for a connection-holder to park on. A handler that must read
outside its transaction commits or rolls back first - `issue_create.go` rolls back at the
duplicate-external branch before it opens the advice transaction - and a scan that publishes
drains its rows first (`scanPendingEvents`, `documentRooms`), because an open cursor holds
its connection until it closes.

**No Envoy listener call runs with a transaction or a pooled connection held.** One caller, one
connection bounds how many of the pool's connections a caller takes; this bounds how long it
keeps the one it has. The listener is a cross-service HTTP call bounded only by its five-second
client timeout (`internal/dispatch/envoy/client.go`), and production runs one Dispatch task on
a pool of `store.sharedPoolSize` connections, so a call made inside a transaction holds one of
them - and the rows it locked - until the listener answers; that many concurrent ones empty the
pool and stall every unrelated request, which a listener restart alone is enough to cause. A
read (`Sessions`, `Role`, `Interest`, `ListInterests`) is resolved first, with nothing held; the
transaction then re-reads under its own lock whatever the resolution depended on and decides
with the resolution only while the locked row still agrees, taking it once more when it does
not and answering a conflict after that. `api/envoy_resolve.go` owns that loop
(`resolveThenLock`, `errStaleResolution`), the delivery read every send resolves through
(`resolveDeliveryTarget`) and the settle half (`settleDeliveryAttempt`, `attemptClaim`,
`claimLapsed`, `deliveryOutcome`, `receiptError`); its callers are `api/comment_create.go`
(the mention and route resolution behind `suppress_route`), `api/comment_delivery.go` and
`api/message_delivery.go` (a delivery's recipient, its claim and its settle) and
`api/issue_claim.go` (the holder's liveness, on both the claim and the release route). A write -
`Envoy.Send`, `Unsubscribe` - runs after a commit, never inside: the attempt is committed first,
sent, and settled by a second short transaction, so a process that dies in between leaves a
record of the attempt rather than losing it.

Both delivery paths are that pair, and the same claim. A comment mention's attempt is the
`comment_deliveries` row the creating transaction commits as `pending`; the post-commit sender -
and every later `POST /api/v1/comments/{id}/deliveries` retry - claims it (`claimed_at`, migration
`0046`), resolves, records that resolution on the claimed row, sends, and records `sent` or
`failed` with its `comment.delivery` event in a second transaction. A targeted message is the
same shape on `message_deliveries`, resolved before the claim rather than after it:
`recordPendingMessageDelivery` claims or opens the attempt and commits it `pending` carrying that
resolution, the send follows, and `completeMessageDelivery` settles it with the `message.delivery`
event. Either way the pending row names the session its frame is going to before that frame is
sent, and each of those statements is its own short transaction or single pooled read, so no
step of a delivery holds a connection across another.

A claim outlives its sender by a minute, which Postgres judges (`claimLapsed`) against the
`claimed_at` Postgres itself wrote, so no task's clock skew can read a live claim as lapsed or
leave a stranded attempt unresumable. A retry beside a live claim takes an attempt of its own;
one beside an abandoned claim resumes the original attempt under its original number. A resumed
attempt keeps the recipient it was opened for - a comment attempt its pinned session or its
resolve error, a message attempt the session its row names - because that is the session holding
the frame; a role that has moved since is reached by an attempt of its own rather than by
re-pointing this one, and only whether the original recipient can still receive the mode is
re-derived. An attempt stranded before anything was resolved has no recipient to keep and is
resolved afresh under that same number, never reported undeliverable unsent.

The idempotency key carries no attempt number: it is `<message>:<mode>` and
`<comment>:<target>:<mode>`, stable across every attempt of that pair. The listener prefixes the
recipient (`agent.<session>.<key>`) to make the envelope's `dedupe_key`, and the JetStream MsgId
appends the topic, so the key's real scope is **(message, mode, recipient session)**, and a retry
of a send that already landed repeats the dedupe key of the frame that landed - and nothing else
of it, because the listener mints a fresh `event_id` per send. That is what makes a retry after a
receipt timeout safe: the listener publishes the envelope before it answers, so an answer that
misses the client's window says nothing about whether the message landed, and only the same key
can be recognised as the repeat it is. **Who recognises it, for how long, and where that fails is
stated once, on `DELIVERY_DUPLICATE_WINDOW_MS` in `packages/contracts/src/dispatch-api.ts`; read
it there rather than here.** The stream's duplicate window equals its retention by construction
(both are `streamDuplicateWindow`, `internal/bus/stream.go`) and is reconciled on every
`bus.ConnectOwningStream` by `ensureStreamWithConfig`.
A retry in a DIFFERENT mode is a different key and genuinely does deliver again, which is what
the dashboard's retry row says: its **Retry** re-sends the attempt's own mode, and the two
mode-change actions say "instead". A mode change never rides on a stranded attempt - resuming it
would publish a second frame under one attempt number - so the claim transaction settles that
attempt `failed` ("superseded by a retry in another mode"), moves its `claimed_at` so the
original sender stops owning it, appends its own `message.delivery` receipt, and opens a new
attempt pinned to the session the stranded row named.

**The window is one number, and the promise expires with it.** `DELIVERY_DUPLICATE_WINDOW_MS` in
`packages/contracts` is the single literal: `contracts.DeliveryDuplicateWindow` is generated from
it for Go, and the hosts' dedupe and the dashboard read it directly. Past that window neither the
stream nor a host remembers the key, so a same-mode retry is a second delivery - which is why the
row stores only the CAUSE of a receipt timeout and never the advice. Every delivery surface
composes the advice from `packages/dispatch/web/src/features/conversation/delivery.ts`, whose
header names the surfaces: it makes the promise only for a failed attempt inside the window, and
an attempt its session answered with an error gets a mode change rather than a Retry
(`DELIVERY_DUPLICATE_WINDOW_MS` says why). The mode-change actions are not gated: they always
deliver. Their clause ("sending in a different mode delivers it again") is added only for a
receipt timeout, the one cause whose send may
already have reached the recipient; `RECEIPT_TIMEOUT_CAUSE` in `packages/contracts` is that
cause's single literal, generated into Go as `contracts.ReceiptTimeoutCause` so the string
Dispatch stores and the string the dashboard keys on cannot drift. This window bounds a delivery
retry only: a broadcast's `idempotency_key` is a `broadcast_idempotency_keys` row that lives as
long as its broadcast, so a repeated create is recognised however late it arrives (the broadcast
paragraphs below).

An attempt the stream recognised records `duplicate` and no envelope id: an earlier attempt
landed, so this one is a repeat (`DELIVERY_DUPLICATE_WINDOW_MS` says who drops it), and it reads
as "already delivered" rather than as a fresh send. The flag rides the attempt read, the
`message.delivery` payload and the comment delivery payload, and every surface that renders an
attempt - the targeted-message card, the comment thread's mention list, and the issue event feed
- reads it.

Because the attempt is committed `pending` before its send and names the session that send is
going to, the session can answer or refuse the frame while it is still in flight - and can answer
it still when its sender dies between the send and the settle. Every attempt a sender settles
carries exactly one delivery receipt, and that receipt says what its row says. A reply that
reports an error records the attempt `failed` and appends the receipt itself, while a reply that
carries a body records `sent` with its `reply_id` and appends only `*.answered`, so the settle
transaction that finds its row already settled owes the receipt exactly when the row carries a
`reply_id`, and builds it from that row rather than from what the send reported. Both statements
a settle transaction makes are scoped to the claim its sender took, so when a lapsed sender and
the sender that resumed its attempt both come back, only the one the row's claim still names
settles it or pays anything on it. Both transactions lock the event's owner before the attempt
row, the order every other delivery transaction takes them in; locking the attempt first would
invert against the claim and deadlock two concurrent retries.

Table row and column deletion records a mark snapshot during prevalidation, then locks the
corresponding ask/comment rows with `FOR SHARE` in the edit transaction before its token check and
Yjs transaction, and re-derives the selected cells' mark set inside Yjs before applying. A reopen
waits behind the row lock or is observed as open; a newly committed anchor changes the mark snapshot
and refuses the deletion; an unaccounted mark with no committed ask/comment row fails closed.

References form one graph. Mentions (`dispatch://` refs and same-origin dashboard URLs in a
document version, ask question, comment body, or issue message) are derived on every write into
`refs` by `refs.ReplaceCounted`, which reconciles rather than rewrites: an edge that survives keeps its
`created_at` and `source_seq` (the `events.id` that introduced it, stamped by `refs.Stamp` right
after the source's event is appended, in the same transaction). Structural relations stay in the
columns that own them and the `graph_edges` view (migration 0032) unions both into one typed edge
relation: `mentions`, `child_of` (`issues.parent_key`), `attached_to` (`artifacts.issue_key`),
`anchored_to` (ask/comment anchors), `owned_by` (project-document asks/comments), `replies_to`
(comment and message threads), `followed_by` (`ask_followers`). Artifact targets are addressed by
`ref_key`, artifact sources by uuid; each arm has the index its `to`/`from` predicate needs.
`GET /api/v1/references?to=|from=` reads the view; `envoy-dispatch rebuild-refs` reparses every
source and reconciles the index (the text is the truth), deleting edges whose source no longer
exists, and refuses to run without `dispatch.server_url`.
A mention's source is the node whose text holds it, never the issue that text belongs to: a
citation in an issue's spec is an edge out of the spec document, so
`GET /api/v1/references?from=dispatch://KEY/spec` lists it, `?from=dispatch://KEY` lists only the
issue's own `child_of` and `affects` edges, and the cited node's `?to=` backlink names the spec.
Where a reference in text ends, and what it names, is one rule with two readers: `text.ExtractAt`,
which every write indexes through, and the dashboard's `composerReferences` (`refs/routes.ts`,
scanning with `referenceSpans` and parsing with `referenceRouteFromHref`), behind its reference
pills, unfurl cards and linked text. `DISPATCH_TEXT_REFERENCES` in `@legion/contracts`, whose
JSON copy is `text/testdata/dispatch-text-references.json`, is the table both are tested against,
so a change to the rule changes both readers and the table: `**dispatch://KEY**`,
`` `dispatch://KEY` `` and `[dispatch://KEY](dispatch://KEY)` (how a document stores
`<dispatch://KEY>`) all mention `KEY`, though the dashboard shows the code span as code, not a
link. The Go reader parses a reference as the browser does: a query's pairs split on `&` alone,
as `URLSearchParams` splits them (`searchParams`), an item id is decoded as `decodeURIComponent`
decodes it, a slug is the whole segment, and a reference holding a control character, or an item
id that decodes to one, names nothing; the index binds ids as `text[]`, where Postgres refuses a
NUL, so a decoded NUL would fail the write and stop `rebuild-refs` at the body holding it. Where a
browser normalizes a URL and net/url does not (host case, a default port, dot segments, a
backslash), the two still differ, and so does how many U+FFFD stand for the invalid UTF-8 in a
query's item id, an id no item has.

The live document holds the tree as the browser editor holds it (`pmdoc.Update`, `pmdoc.Read`).
That editor builds each node it loads with its schema, so an attribute the live document lacks
takes the schema's default, and it writes a node's attributes back when the node is edited. Where
the tree's null is not that default, the live document holds a value the editor keeps instead
(`liveNulls`): `"none"` for a table cell with no alignment, whose default, left, would left-align
the column at its first edit, and `""` for a code block with no language and an image with no
title. A read gives each back as null. `pmdoc/gen/decode.ts` decodes Go-written bytes into the node
the browser holds, schema defaults applied, so `TestUpdateFromEmptyEqualsAuthoredByBrowser` fails
for an attribute of this kind `liveNulls` lacks. It cannot tell an absent attribute from the
editor's value, since the editor shows its default for both; `TestNullAttributesSurviveABrowserEdit`
edits a node of each kind through the editor's sync plugin (`pmdoc/gen/edit-blocks.ts`), which
writes the editor's values back, and requires the read to give each back as null. Every gen script
a test reads hands its result back in a file the test names, never on stdout, which a Bun script
can cut short while exiting 0 (`docs/solutions/testing/bun-console-log-drops-what-a-full-non-blocking-pipe-cannot-take-and-exits-0.md`).

A document `pmdoc` refuses names the first refused block it writes. One walk of the tree goldmark
reads decides every block refusal in document order before conversion (`refuseBlocks`), and it
refuses every block kind conversion does not convert (`convertedBlocks`), a link reference
definition among them, so conversion refuses no block; `TestParseNamesTheFirstRefusedBlock` holds
that order for every pair of block refusals. Spacing refusals (`browserListSpacing`) come before
the walk, and inline ones, such as a footnote reference, in conversion after it.

Typed document blocks are declared only in `internal/dispatch/pmdoc/schema/blocks.json`. The
embedded file is the server-owned schema, `GET /api/v1/schema/blocks` returns its exact JSON, and
the fixture generator reads that checked-in file. A typed block is CommonMark generic-directive
syntax: `:::name{#block-id key="value"}` followed by block children and a closing line of colons.
Both parsers close it at a line of at least as many colons as the opener, indented less than four
columns past where its lines start, whatever block inside it the line would otherwise continue - a
paragraph, a list item or a fenced code block - so the renderer escapes a lone `:::` in text
written within that reach of a typed block fenced with three colons, and at a typed block's own
prefix whatever its fence, and writes three colons, or one more than the longest such line inside
the typed block (`closingColons`): a nested typed block's fence, or a line of code, measured in the written line's
columns - the width of the list markers and `> ` around it, and a tab advancing to the next
multiple of four from the column it stands at. A callout nested directly in a callout is written `::::callout{…}` …
`::::`, as the browser editor writes it. There is
no whitespace between `name` and `{`; Pandoc fenced divs, leaf directives, and text directives are
invalid outside code blocks: a line opening with one is refused where it could open a block, and in
a paragraph wherever it stands. One check reads each line of a paragraph two ways and names the
first line either refuses (`paragraphDirectiveReason`). As written, past its containers' prefixes
with the spaces and tabs of its indentation trimmed, a three-colon opening on any line after the
paragraph's first is refused, naming the line's number in the markdown the caller wrote, front
matter counted, and its text: it continues the paragraph - four or more columns past its
containers' prefixes, or in a replace's inline text - so goldmark reads it as the paragraph's text,
and its author wrote a block. As the browser editor's parser reads it, each line of the paragraph's source with the
whitespace it opens with trimmed, so that only a quote's marker opening the line keeps it text, a
line opening with three colons passes only as `:::` alone or a three-colon opening, a four-colon one
included in what it refuses. The renderer never writes the first shape (it escapes a line-start
opening and encodes leading spaces as `&#32;`), so no stored rendering reads back refused; an escaped
opening, or one inside a line, is stored as text and reported in the write's advice
(`unparsed_openers`). An unclosed typed block at document level is rejected, while one nested
inside another block runs to that parent’s end. A typed block's lines start where its opening line's
text does: both parsers take up to that many columns of indentation off each of its lines, as off a
fenced code block's (`typedDirective.indent`), so a typed block nested in an indented one closes,
and every block inside is read, from there.

Text a caller writes reaches the parser with line feeds alone: `pmdoc.LineFeeds` writes each CR LF
and each lone carriage return as a line feed, as CommonMark and the browser editor's parser read
both, in `ParseForWrite` (a spec, an upload, an insert) and `ParseInline`; in every edit
operation's text before any check reads it (`applyOperation`: a replace's `with`, an insert's
markdown whether it becomes blocks or table rows, a retype's attributes); in a suggestion's
`replace_with` when it is created and when it is accepted; in the attributes written onto a
typed block (`SetBlockAttributes`, `pmdoc.LineFeedAttrs`), and in an answer's text, which its
ask block carries; and in a block ask's edited question and options. The browser editor's own
updates cannot carry a carriage return. No stored document holds one, and `pmdoc` handles line
feeds alone. Marks are read as that parser reads them, as a set, where goldmark nests them: a mark
opened where the same mark is already open adds nothing, and its close ends the mark for the rest
of the text around it, up to the node that opened it (`parseInlineMarks`), so `*x *y* z*` is
`x y` in emphasis and ` z` without, and `****a****` is strong once. An image is read without the
marks around it, a link among them, as the browser editor's store (y-prosemirror, which keeps a
mark on text alone) holds it, so a linked image is stored without its link (LEGION-365) rather than
refused. Delimiter runs pair as that parser pairs them: CommonMark's rule of three is judged on the
lengths two runs have left after the pairs already made from them (`emphasisDelimiters`), where
goldmark judged the lengths they were written with, so in `***a.****&#32;b*` the closer's last `**`
is text. A run of `*` or `_` can also open where another `*` or `_` follows it and close where one
precedes it, as that parser's attention markers let it (`emphasisParser`), where goldmark's
flanking rules alone decide, so in `b_*a**` the `*` can both open and close, the rule of three
keeps it from the `**`, and the line is text. That parser counts `~` as such a marker too, and Go
does not: beside a `~` its strikethrough resolver pairs runs apart from and sometimes before the
`*` and `_` runs, which goldmark's one delimiter stack does not model, so the `~` half alone would
misread `[**~~**d**~~**](u)`, which both parsers read as strong struck `d` under the flanking rules.
The writer takes a spelling that reads back under CommonMark's flanking rules as well before one
that reads back under that parser's alone (`inlineWithEscapes`), so it keeps the bytes it wrote
before the marker rule. A code span keeps the whitespace that
starts each of its later lines past the prefix of the containers around it, as the browser editor's
parser reads it, a line holding only whitespace before the closer included; goldmark's paragraph
trims it (`lineRecordingParagraph`, `multilineCodeSpanText`). A space or a line feed is the padding
such a span sheds at each end (`codeSpanPadded`) where something else stands between, and the
writer pads a span whose text starts and ends with one. The columns left of a tab a container's
marker took part of are text to that parser, not spaces, so they stand between, and a span ending
in them sheds nothing. The writer writes a span's later line as it is, without the containers'
prefix, except where a list item or footnote definition would take columns off the whitespace it
opens with, and there behind the prefix (`takesCodeLineIndent`). The spaces and tabs a line of text
ends with are dropped, as that parser drops them, and are a hard break only where they are two
spaces or more and no tab (`trimLineSuffixes`); goldmark kept all but the last and broke at any two
spaces. A backslash ending the line keeps what stands before it. A lazy continuation line - one that
continues a paragraph in a list item, a quote or a footnote definition without the container's
prefix - is never a table's header or delimiter row (`lazyTableRows`), as in GFM, and a table one
would be a body row of is refused (`markLazyRows`): goldmark continues the paragraph the table is
made of with the line, where the browser editor's parser ends the table, and every container the
line does not continue, before it. So is a table a line opening another block would be a row of - a
list item that cannot interrupt a paragraph, whatever its marker, or indented code
(`markBlockRows`): goldmark's table is a paragraph, which such a line continues, where that parser's
table is no paragraph and ends there, reading the line as that block. So is a table a body row of
which holds text in a cell past its delimiter row's width (`markWideRows`), naming the row and both
fixes (a pipe inside a cell written `\|`, or header and delimiter rows as wide as the row):
goldmark drops the cells past the table's width, where that parser keeps them, and a cell ends at
every `|` not written `\|`, inside code and links too, so a code span holding a bare `|` in a row
that fills its table lost the rest of the row's text when it was stored. A row whose cells past
the width are blank loses nothing and is read at the table's width. A bare-row insert
(`InsertTableRows`) decides in three steps. First, its fragment, less its blank lines at either
end, must have every line yield a cell and an unescaped `|` (a lone `|` yields no cell, and a line
whose only pipe is `\|` has no separator), with no line delimiter-shaped (cells of three hyphens
or more, `tableDelimiterRow`). Each line is trimmed there as goldmark trims a row (`rowSpace`:
space, tab, line feed, carriage return), so `|` then U+00A0 is a row whose cell holds it. Second,
`parseTableRows` parses such a fragment under a header it writes at the target table's width, and
what that parse refuses is refused: a wide row of the table under that header is `markWideRows`'
refusal answered as `TABLE_WIDTH`, and anything else keeps its own refusal, `INVALID_OP` on
`markdown` (a line `markBlockRows` refuses, such as indented code or `2. | a | b |`, however many
cells it holds, since the refusal walk reads that before the width, or a table the fragment makes
itself). Third, when that parse refuses nothing and reads one table holding every line, those rows
are inserted. Every other fragment goes to the block path whole, which reads it as a document of
its own and writes whatever blocks that reading makes, paragraphs or a table of its own (a line of
hyphens, one or more a cell, is a delimiter row there), or refuses them, so its answer can differ
from an upload of the same lines under the target.
Goldmark also drops a row's closing `|` whatever stands before it, where that parser reads one
after an odd run of backslashes as the last cell's text, so that pipe is put back in the cell
(`keepEscapedClosingPipes`). The renderer writes a cell's pipe `\|` and the header as wide as the
widest row (`tableGrid`), so no rendering holds a wide row. A setext underline under a
table is the table's row, as that parser reads it (`underlineAfterTable`), all but a lone `-`, an
empty list item there: goldmark's setext heading took the table's paragraph, then wrote the
underline as a paragraph after the table, or made the lines before the table a heading after it.
Under a lone `-` goldmark's own handling still does that where text stands before the table in its
paragraph, making the text a heading after the table where that parser reads the paragraph, the
table and an empty item, so such a document is refused (`underlinedTextAttr`).
A tab in a line's indentation spans the columns to the next multiple of four from where it stands,
as CommonMark and the browser editor's parser read it, so after a quote's `> ` it spans two: `> \t- a`
opens a list, `> \t| a |` over `> \t| - |` is a table, and `> a` over `> \t===` a setext heading
(`tabIndented`, `tabExpandedLines`). Goldmark measured such indentation as if it began the line, or
took a list marker or an underline only after spaces, and read each as paragraph text.
An indented code block right after a list, outside it - which only a last item holding its content
five or more columns in allows, by a wide ordered marker, spaces or tabs - or right after a quote,
on the line after the quote's last, is refused when it holds more than one line: the browser
editor's parser keeps the list or quote open across the code's first line, which it does not
continue, and reads the code's later lines as a second code block. A blank line before the code
ends a quote, so there the code is read whole.
A fenced code block whose language - the info string's first word - holds a backslash escape or a
character reference is refused: that parser decodes both, and goldmark keeps them as written. What
it leaves as written (`\q`, `&bogus;`) is read as before, as is the rest of the info string, which
both drop.
A task list item's marker (`[ ]`, `[x]` or `[X]` opening a list item's first paragraph) is read as
the browser editor's parser reads it (`taskList`): followed by a space or a tab and then more text on
the line, or by a line ending the paragraph continues past, and it takes only the one character
after it. Goldmark's took the marker with anything after it, so it read a link opening a list item
(`- [x](https://…)`) as a checked task.

Front matter is read as the browser editor's parser reads it (`pmdoc.parseFrontmatterBlock`): a
first line that is `---`, with any spaces or tabs after it, opens it, the first later line that is
the same closes it, and its text is stored between plain `---` fences with line feeds between its
lines. A `---` opener nothing closes is a thematic break. That parser, having tried such an opener
as front matter to the document's end, reads no list, quote or footnote definition at the
document's level in the rest, since no container opens inside front matter; the lines read as the
other blocks they make, so `---\n- a\n- b` is a rule and one paragraph holding both lines. Parse
reads them so too (`pmdoc.frontmatterAttempt`); a typed block's content opens them as anywhere.
So the renderer writes a rule that opens a document as `***` where `---` would be misread - a
later `---` line would close front matter, or the document holds a list, quote or footnote
definition at its level (`holdsAContainerTheBrowserDrops`) - and `---` everywhere else.

Two lists of one kind side by side read back as one when written with one marker, so the
renderer writes a list whose kind matches the block before it - past an empty paragraph, which
it writes as nothing - with its kind's other marker, `*` after `-` and `)` after `.`, alternating
as the browser editor does (`otherListMarkers`); a list anywhere else keeps `-` or `.`. An edit
that leaves two lists side by side (deleting or emptying what stood between them, inserting or
accepting a list beside one) therefore stores the two lists it made, and a `replace` refusal that
names a list item's marker names the one it is written with (`BlockMarker.Other`).

The renderer writes a run of inline text so that it reads back as written. A bare URL ends where
linkify stops, so where a run does not read back because linkify would continue a URL into the
character after it, that character is written behind a backslash (`endsBareURL`) - a backslash even
for `&` and `~`, whose other escapes are character references, which linkify runs through. Marks are
written in one order - link, strong, emphasis - so where a text still carries a mark the text
before it opened, and the order puts that mark after the text's other marks, the mark is closed and
opened again; where that puts two runs of one delimiter character side by side (bold inside italic:
`*`, `**` and `*`), the parser reads one run. Such a run is kept where it reads back, as the parser
then splits it as written, and one that does not is written again in the plain respellings, as main
writes it again. Only where none of those reads back is it written with the marks still open kept
open, then the marks the next text still carries, and the text's others opened inside them
(`keepingOpen`), kept only where that reads back with nothing fused, so `_**a** b_` is written
`***a** b*`; a run that fuses nothing is written as before, so italic closed around a link keeps
its bytes.

A container that holds nothing is read as the browser editor's parser reads it, holding one empty
paragraph (`emptyParagraphFirst`): an empty list item (`-`), quote (`>`), typed block or footnote
definition, and a list item that opens with another block (`- # h`) holds an empty paragraph ahead
of it. A table with no body row holds one empty row, which the renderer writes as nothing. The
renderer writes a list item's empty first paragraph as nothing, with the next block on the
marker's line, a rule there with the other character from the marker's, `***`, and `---` after
`* ` (`- ---` and `* ***` are thematic breaks at the list's level). A task
item cannot be written so, since its marker's line would carry the next block as the task's text
and the browser reads no other form of it as a task: a task item whose emptied first paragraph has
another block after it does not render, and an edit that would leave one is refused.
An empty list item that would interrupt a paragraph is not opened, as that parser reads it on the
whole line (`emptyItemGuard`): after `- a`, the line `  - -` is an item holding the text `-`. Nor is
an ordered item numbered anything but a lone `1` (`orderedCannotInterrupt`): goldmark takes the
number's value and so lets `01.` interrupt a paragraph, where that parser reads `a` over `01. b` as one
paragraph. Neither is opened on a line after indented code, blank lines between or not, whatever
containers the line opens first, since that parser holds the code open to that line and decides
once per line whether it interrupts - except code right after a list, which that parser ends on its
own line (`interruptsOpenBlock`): `    code\n> 2. b` is a quote holding the paragraph `2. b`, and
`1.\n\n    code\n2. b` is two lists with the code between. A table is no paragraph to that parser,
so after one such an item opens in a container the line opens first (`| a |\n| - |\n* -` is a list
item holding an empty one). An item whose marker line holds nothing
takes its content from the next line when that line reaches its content column with no blank line
between, a list marker there included (`emptyItemGuard.Continue`): goldmark closed the list for a
marker that cannot continue it, so `-\n  1.` read as two lists.

Every document's lists are spaced as that parser reads them (`browserListSpacing`): outside
quotes and footnote definitions a blank line between two items spreads the list, and one between
an item's blocks spreads the item; in a footnote definition a list is never spread, and an item is
spread by a blank line between its blocks or after it, before the next item or a quote or list the
definition goes on with; in a quote a blank line after an item spreads the list, and so do blank
lines after its last item, one before a quote or a list (or anything, in a typed block inside the
quote) and two before anything else, or at a typed block's fence - where no fence of its own closes
a typed block, the fence of the typed block it stands in (`fenceEnding`) - three at the quote's end
where no fence ends the typed block, but for a container opening on the line right after them, and
one more of each after an item ending in a quote in the typed block (`quotedListSpread`,
`blanksEndingQuotedList`), and where a typed block inside the list's quote stands in a quote that
goes on past them, the blank lines of the quotes around the list's after one of its own count among
them (`blanksThroughOuterQuotes`). A typed block's content is a document of its own to that parser,
ending where its fence or the block around it ends it, so where quotes and typed blocks nest around
a list more than once a blank line in its quote at or after it, outside the code it holds, is
refused (`quoteTypedAlternations`); blank lines after an item that ends in a quote or a list are
that block's, and in a quote those after a footnote definition an item holds are the definition's -
one spreads the item only before a quote, a list or a definition, and two before anything
(`definitionBlanksInQuote`), the same after an item ending in one, where the next item and the
quote's end count as anything, and none spreads the list (`definitionEndSpreadsItem`,
`blanksAfterDefinitionItem`), an empty definition's own line being one of them (`emptyDefinition`,
so `> - [^m]:\n>` is a spread item); and a quote and a footnote definition together mix the two
(`footnotedQuoteListSpread`). A blank line after a fenced code block no fence closed is the code's
and spreads nothing, even where the code's text drops it, unless a quote between ends at it
(`keepsBlankLinesAfter`). The code's text drops that line where it ends a list item or a footnote
definition that flow content follows (a paragraph, a heading, a rule, a fence, a typed block or a
table), or a quote no container opens right after, and keeps it before a quote, a list item or a
definition, as that parser reads it (`blankTaker`). A list item decides by what follows its list only
inside its own quote or typed block; past one, that container's rule decides, so a typed block's
fence keeps the line and a quote's end judges it by what opens right after. The renderer writes each spacing so that it reads back
(`blanksAfterList`, `writesBlankAfterItem`): no blank line before a block that opens on the line after
a list where one would spread what it follows, none inside a list item after a list ending in
an empty item, where goldmark ends the item at a blank line, and none in a quote after a footnote
definition ending in a list no line continues, where the reader refuses one (`endsInClosedList`); a footnote definition a tight list item
holds writes its blocks with the item's tight lines, since that parser reads them as the item's
(`itemBlocks`). Where the lines alone do not decide
the spread - a typed block holding a blank line in a list item, a blank line at the end of a quote
after a list, or at or after a list in a typed block in a footnote definition, whatever quotes or
typed blocks stand between - a document holding
one of those shapes, or a footnote definition that ends in a block other than a paragraph, is
refused, and so it is where goldmark reads its blocks otherwise: an empty list item and a blank
line before a block an item around it holds, however far out, since goldmark ends every item around
the empty one there (`emptyItemEndsOuterItem`), an item's content starting one column past its
marker where its marker line holds nothing more or indented code (`itemContentColumn`). Every other document keeps goldmark's looseness there -
a loose list's items holding more than one block are spread, the list when none is - which is how
the documents Dispatch stores were read; but a blank line at or after a list in a typed block in a
footnote definition is refused there too, unless goldmark spreads every item a blank line follows
(`goldmarkSpreadsItemsBeforeBlanks`), since that parser spreads such an item and never the list.

A footnote definition is read where it is written, as that parser keeps it: inside another block,
ahead of other blocks, in any order, and whether or not anything refers to it. One inside another
footnote definition is refused, since that parser reads a line of `=` or `-` continuing the inner
one's paragraph as a heading's underline, where CommonMark reads it as the paragraph's text, and so
is one inside a typed block, which that parser's references reach only from inside a typed block or
after it. A definition whose label holds whitespace is refused, which that parser reads as a
paragraph. A label is stored as that parser reads it, its escapes and character references decoded
(`[^a\*]` is `a*`, `[^f&amp;g]` is `f&g`), while a reference still finds its definition by the
label as written; the writer escapes a bracket, a pipe, a backslash before punctuation or at the
end, an ampersand opening a character reference, and white space as a numeric one, so each label
reads back and one that needs none is written as it is (`escapeFootnoteLabel`). A reference whose
label matches its definition's only as written, not once both are decoded (`[^&AUML;]` beside
`[^&auml;]: `, whose written forms fold to one key while `&AUML;` names no character), is refused,
since written from the decoded labels it would no longer find the definition. A definition's later
lines start four columns past where its container's content starts, whatever indentation stands
before its `[^` (`browserTextColumn`, `quoteContentColumn`). Goldmark gathers each definition, as
it closes, into a list the parser keeps ahead of every block written while the document parses, so
a check of the block before another meets the block written there (`footnoteDefinitionParser.Close`),
and the parser puts each definition back where it was written and removes the list once the
document is read (`definitionsInPlace`). Goldmark's footnote transformer, which would order the
definitions by first reference, drop the rest and append backlinks, is not used.

A typed block renders its `blockId`, defaulted attributes, and every explicitly set optional
attribute. Parsing mints an omitted id, while live document reads and writes validate each node
against its schema content rule. Schema changes are additive: add a type, add a defaulted
attribute, add an enum choice, or widen a content rule. Tightening content, removing or renaming a
type or attribute, or requiring a new attribute requires a document migration and version bump.

`ask` blocks are indexed at settlement: their body and client-owned attributes update the ask row,
the row restores server-owned answer state into the block, and removal retracts the indexed ask.
The block is therefore the source of truth for an ask's **text** (question, options, `multiple`,
`urgency`) and the row for its **lifecycle** (`state`, `answer`, `resolution`), so a route that
changes either writes both: `PATCH /api/v1/asks/{id}` on a block ask writes the block through
the document ledger and then takes the row's values from what the block parses back to, and
`POST /api/v1/asks/{id}/resolve` writes the block's `state`. An edit writes only the fields it
names: the row holds the question as plain text, so rebuilding the whole body from it would
flatten an untouched question's formatting and links and mint fresh ids for the paragraphs a
comment anchors into. Naming `urgency` or `multiple` alone therefore goes through the attribute
path and leaves every child node, mark and inner block id exactly as it was; naming `question`
or `options` replaces that part with what the markdown pipeline parses, so it carries the same
list attributes any other document write gives it, and anchors inside the text actually replaced
are affected as they are by any document edit. A field named but unchanged is not rewritten, so
an idempotent retry of the whole ask writes nothing, versions nothing and keeps every anchor.
Text the block cannot carry unchanged is refused `400 ASK_BLOCK_TEXT` naming the field, with
nothing written. The rule is a round trip through settlement's own parser and the canonical
markdown a version records, never a list of forbidden characters: whatever the block cannot carry
back unchanged is refused, and an option label containing `": "` - the separator between a label
and its description - is one example rather than the only one. A single newline is
carried as a hard break; surrounding whitespace is trimmed, as the parser trims it.
Settlement retracts an ask whose block left the document in its own name,
`{kind: "system", id: "document-settlement"}`, and restores only a retraction it wrote - a
person's or a session's retract stands however the document moves, which is why `resolve`
refuses a caller reason beginning `removed from the document in version`.
An invalid browser-edited ask retains its indexed ask, carries the server-owned `invalid` parse-error
attribute, and emits `block.invalid`; repairing its body clears `invalid` before updating the ask row.
Markdown that becomes a whole document - a spec at issue creation, an uploaded document or version -
is refused with `400 INVALID_ASK_BLOCK` when an ask's body breaks its content rule,
`paragraph+ bullet_list?` - one or more paragraphs, then at most one bullet list, last
(`pmdoc.AskContentError`) - as the browser editor's parser refuses to build such a block. A new
document is held to it for every ask, and so are a version that repairs a stored tree outside the
schema, which has no readable asks to compare with, and a rebuild from supplied markdown; any other
new version only for each ask it writes or changes
(`refuseChangedAsks`), comparing the ask's rendering with the current one (`newAskMarkdown`, the
asks of one check sharing one budget of span cells, spent in document order as the document's own
render spent it, so a live ask over a table with colspans or rowspans matches the cells its stored
markdown wrote them out as while that budget lasts), since a
version is markdown and cannot carry a comment's anchor mark or the id a reader's browser derives
for a heading; what the rule allows is taken, and an option without a label or a question
that is only an image is left to settlement's `invalid` flag. A document edit is refused for an ask
it writes or changes that breaks the rule or that settlement cannot read (`validateEditedAskBlocks`).
Neither refuses an ask a browser edit left unreadable that it carries through unchanged, so such an
ask does not refuse edits or versions elsewhere in the document, except that a new version is
refused for an unreadable ask in two cases. Where only the asks' tables have spent the budget of
span cells (100,000) before an ask, it is refused when its table's markdown holds a body row
shorter than its widest, which the parser pads when it reads the upload (the header is always
written as wide as the widest row): a span-free table with a short body row, or a span table left
with one once that budget ran out, such as a body cell spanning columns under a wider header, a
rowspan, or rows the budget ran out partway through. Where a table outside the asks spent some of
it first, the check can write span cells the document did not, and the ask is refused unless
padding its stored rows gives those cells.
An answered block carries `state`, `answered_by`, `answered_at`, `selected`, and `answer` in
canonical markdown.

## Critical conventions

- `packages/contracts` defines the TypeScript event schemas consumed by Dispatch clients. The Go Dispatch server maintains its emitted event names and wire payloads separately; `bun run gen:go` generates Envoy envelope validation only.
- `GET /api/v1/issues` answers every issue its filters match (`project`, `status`, `parent`, `label`, `priority`, `open`, `updated_since`, `route_status`, and the human-only `pinned`) as an array, or one page of them when the caller names `limit` (1 to `contracts.MaxIssuePageLimit`, 250) or `offset` (0 or more; alone it pages `contracts.DefaultIssuePageLimit`, 50): `{issues, total, limit, offset}` (`model.IssueSummaryPage`), the count beside the rows as `GET /api/v1/asks/open` carries it. `parseIssuePage` (`api/issue_page.go`) reads both, and a repeated, blank, non-integer or out-of-range value is `400 INVALID_QUERY` naming the parameter; so is `cursor`, which the listing does not page with, since answering it with the first page would hand back rows the caller cannot tell from the ones it asked for (LEGION-406: the route ignored all three and answered every row). The page is cut after every filter, `route_status` included, which is applied in Go after the query, so `total` is what the filters match. The order ends on the key, the one column no two issues share: nothing makes a rank unique (`issues_project_rank` is a plain index, and every project's first issue is `U`), and `created_at` is the creating transaction's start, so without the key consecutive offsets could show a row twice or never even over a listing that holds still. A walk of the pages is exact only on an unchanged listing: an issue that enters or leaves what the filters match, or whose status or rank changes, between two reads shifts rows across a page boundary, and one issue is served twice and another never. The unpaged array is the only exact set one read gives. The `dispatch_issues` tool always sends `limit` and `offset` (`DispatchClient.listIssuePage`, `packages/envoy-client`) and refuses an array answered to that paged request, which means a Dispatch older than #1612 or a regression of the page. So a Dispatch rolled back past #1612, or one that stops paging, fails every `dispatch_issues` call with that refusal; the SPA and both daemons send neither parameter and keep reading the array.
- Issues carry a nullable coarse priority (`P0` highest through `P3` lowest) alongside their server-generated fractional `rank`. `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` accept `priority` as `0` through `3` or null; each priority write emits `issue.updated`. Issue and pinned lists sort by lifecycle status, then rank, then creation time, then key; priority is a badge and a filter, never a sort key, so the List and the Board (whose columns keep the list's order) show the same order. `PATCH /api/v1/issues/{key}` accepts neighboring issue keys as `rank.before` and/or `rank.after`, validates they share the project, and serializes rank allocation per project before it rewrites only that issue's order key.
- Every issue has a nullable `parent_key` (`graph_edges.child_of` is a live view over it). `POST /api/v1/issues` validates a supplied `parent` exists in the same project (`400 PARENT_INPUT`), and `PATCH /api/v1/issues/{key}` accepts a tri-state `parent`: absent leaves it, `null` clears it, a key reparents after locking the issue and the proposed parent in key order and refusing a missing, self, or foreign-project parent (`400 PARENT_INPUT`) and a cycle (`409 PARENT_INPUT`, with a depth-capped `union` ancestor walk so reads terminate even if a raced reparent ever commits one; the pairwise lock leaves the multi-ancestor race accepted, like rank). A reparent of a closed issue is `409 ISSUE_CLOSED` (a closed issue takes only `rank`, `components`, and a reopening `status` — any status but `done`; every other field, `priority` included, waits for the reopen). An actual parent change appends `child.removed` to the old parent and `child.added` to the new one (`{child_key}`, all owners in one `LockOwners` with the issue's own event) — both always notify, like `child.status`. Issue-detail `children` rows are one recursive-CTE query (`loadChildren`): each direct child carries `subtree_done` / `subtree_total` (the child itself included, every status; done = `status='done'`), `active_at` (the subtree's newest `updated_at`), and its own `external_links`, still ordered by key.
- Every issue has a nullable `assignee`: the **lowercase** GitHub login of the human who answers its asks (`issues.assignee`, migration `0034`, indexed on open issues). `parseAllowedLogins` lowercases `DISPATCH_ALLOWED_LOGINS` and the identity implementations compare lowercased, so `api/issue_assignee.go`'s `canonicalLogin` (`strings.ToLower(strings.TrimSpace(login))`) is the stored form and plain `=` compares it; `/auth/whoami` still echoes GitHub's display casing (`Xodarap`), so the SPA lowercases the viewer once and compares exact. `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` accept `assignee` (the PATCH's `json.RawMessage` tri-state: absent leaves it, `null` clears, a string is canonicalised and must be an allowlist key — else `400 ASSIGNEE_NOT_ALLOWED` naming the login; a non-string is `400 INVALID_ISSUE`). Any authenticated actor may set it; the event's actor is who assigned it, and the whole issue rides in `issue.created` / `issue.updated`, so there is no `assigned_by`. Absent on create, the default is the first match of: a human actor's login; a personal token's `Owner`; the parent's assignee (which may be null); null — so the shared token's parentless issues are unassigned and a daemon status PATCH (no `assignee` key) never touches it. `GET /api/v1/users` (human-only) returns the allowlist keys sorted as `{users: [{login}]}` — the picker's options, a pure config read; `GET /api/v1/whoami` (any auth) returns `{kind: "user", login}` for a cookie/header caller or `{kind: "agent", owner}` for a bearer (`owner` is the personal token's lowercase login, null under the shared token) — what `dispatch_whoami` reports. Issue reads (`GET /issues/{key}`, summaries, pinned) carry `assignee`, and so does the `issue` on every inbox row.
- Every issue has a nullable claim: the session that intends to implement it (`issues.claimed_by` — the actor JSON — and `issues.claimed_at`, migration `0045`, both set or both null by `issues_claim_complete`). It is on every issue read (`Issue.claim`, `IssueSummary.claim`, so the detail, the listing and the board rows all carry it) and is neither the `route` (where messages go) nor the `assignee` (the human who answers the asks). The whole rule lives in `api/issue_claim.go`. `POST /api/v1/issues/{key}/claim` (any authenticated actor) claims it: the claimant is the request's own actor, the same identity every other write carries — a human's login from their signed cookie (a body naming a session is ignored for a cookie caller), or, for a bearer, the session the caller declares in `actor`, since a token proves its owner or service subject and not which session it runs. A bearer can therefore name another session here exactly as on any other write; what makes a claim trustworthy is that `dispatch_claim` fills `actor` from the host's own runtime, so no model picks it, and that there is no second parameter for claiming on someone's behalf. The body rejects unknown fields, so an invented one is refused rather than ignored. It succeeds when the issue is unclaimed, when this actor already holds it (idempotent: the claim keeps its original time and appends no event), and when the holder is a session the Envoy listener no longer lists as live (`fetchLiveSessions` takes that snapshot of the same live list `GET /api/v1/agents` serves, with no transaction or pooled connection held, and `claimBlockedBy` — the whole taking rule, in one place — decides from it under the issue's row lock), which records the takeover. Against a live holder it answers `409 ISSUE_CLAIMED` with the claim in the body and the holder's session, live title and claim time in the message; a human's claim, which has no session to be running and none to message, is refused with the person's login and no liveness lookup at all. `{"force": true}` takes either anyway and is human-only (`403 HUMAN_ONLY` for a bearer), and only this route has that force — a refusal on the release route never offers one. An unreachable or unconfigured listener is `503 ENVOY_UNAVAILABLE` rather than a guess at whether a session ended, and a closed issue is `409 ISSUE_CLOSED`. A holder that changes twice while one request runs is `409 CLAIM_CONTENDED` carrying the claim the row shows: nothing was applied, and nothing about that holder's liveness was established, so it is never `ISSUE_CLAIMED`. `DELETE /api/v1/issues/{key}/claim` releases it: the holder, any human, or anyone once the holding session is gone; releasing an unclaimed issue is a 200 no-op. Both answer the issue. Claiming and releasing never move the status, and no status write ever claims (a move to `done` is the one that touches a claim, clearing it), because humans may track work by the status; closing an issue is the one write that clears a claim, in the same `PATCH` (`issue_patch.go`) that closes it. `issue.claimed` and `issue.released` carry `IssueClaimEventPayload {key, status, claim, previous_claim?, reason}`; the outbox publishes both to the previous claimant's own `notifications.agent.<session_id>` topic (`publishPreviousClaimant`), whatever the issue's route and regardless of `notify`, so a session learns it no longer holds the work.

- Open asks accept `PATCH /api/v1/asks/{id}` from their asking session or any human. Each edit carries the full current ask, prior mutable fields, and its editor in an `ask.edited` event; `edited_at` is nullable until the first edit. Ask anchors are set on creation and are not editable through this route. `GET /api/v1/asks/{id}` returns `edits`, every rewording read back from those events oldest first (`{previous, edited_by, at}`). A human answer must carry the `edited_at` revision it reviewed; a mismatch returns `409 ASK_EDITED` without closing the ask.
- Every document version or transactional live mutation refreshes each open anchored ask and comment from the current tree, once per tree: a version written in the transaction whose own live mutation produced that tree inherits that mutation's refresh rather than repeating it, and a version with no live mutation of its own - settlement, a standalone named version - refreshes for itself. A changed persisted anchor emits its own full `ask.anchor_refreshed` or `comment.anchor_refreshed` event in that same transaction; an unchanged row emits none. Refresh events are retained and sequenced on the row's owner topic but never notify or author/follower-route a session: the mutation is a side effect, not an interaction addressed to someone. The refresh writes only the two fields it owns, the quote and the orphan flag, never the whole `anchor` column: it reads every open row up front and writes each one back after the lookups and event appends the rows before it cost, so a whole-column write would erase what another writer put in that anchor in between - the block id `BackfillAnchorBlocks` pins (LEGION-149).
- The quote a refresh reads is the first contiguous run of the row's mark (`pmdoc.FindMark`), so text written inside an anchor must carry its mark. A `replace` through the edit route, and the text an accepted suggestion writes inline or into code, takes every comment, suggestion and ask mark that covers all of the text it replaces (`pmdoc.AnchorMarksCovering`, the accepted suggestion's own mark excepted): replacing a word, the first or last word, or the whole quote leaves the anchor over the new text, and the refreshed quote is its whole current extent. A replace that runs past an anchor's edge rewrote text outside it too, so that anchor keeps only the text the replace left alone, and one covering the whole anchor and more orphans it. A block replacement from an accepted suggestion takes no mark, since it can land a code block an ask's mark cannot cover.
- `POST /api/v1/issues/{key}/asks` and `POST /api/v1/artifacts/{id}/asks` create questions: the asker supplies the options and no option label carries a server rule (a human to-do is the to-do phrased as the question, with whatever options fit it). `kind` may be absent or `question`; `kind: "action"` (removed; migration 0035 folded every stored action ask into a question keeping its options and its answer) and `kind: "approval"` (server-created by the document-approval route only) answer `400 ASK_KIND_INPUT`.
- Document approval is a human review pinned to a version, the way a pull-request review is pinned to a commit. `POST /api/v1/artifacts/{id}/approval-requests` `{summary?}` (any actor) opens an ask of `kind: "approval"` with the fixed options `Approve` / `Request changes`, naming the document and its latest settled version in `ask.approval`; its wording cannot be edited. Its question is `Approve <name> (version <N>)?`, followed by the request's `summary`: what the human is approving and nothing else, since an approval request carries nothing new. A summary is trimmed and must hold text (`400 SUMMARY_INPUT`), and one that would take the question past the ask cap is `400 CAP_EXCEEDED` naming `summary` and the characters left for it, counted against the longest version the request can reach (ten digits, the most `asks_approval_kind_check` admits), so no later version move takes the question past the cap; both are checked on every request, including one that opens nothing, and a request without one gets the bare question. An open approval ask follows every document version in the same transaction, preserving its thread and summary: the move rewords its question to the new version, stamps `edited_at` and appends `ask.edited`, while `requested_version` remains the version the agent last handed to the human, so a moved request is Waiting on agents. Only the move that takes the request from the human wakes anyone: a later move, while `requested_version` is already below the version it named, carries `quiet: true` (`model.AskEditEventPayload.Quiet`), so `events.Broker.Notify` records it with `notify` false and the outbox routes it to no follower, as a human's unnamed `artifact.version` is recorded; the log and SSE still carry it, so a person typing in the document wakes the asker once rather than at every settled version. Calling the request route again reads the open row's `waiting_on` once, before it writes anything (`renewApprovalAsk`, `api/reviews.go`). While it is `agent` - the request moved, or a thread reply newer than its last hand-back holds the turn - the call hands it back to the human: a new summary first rewords it the same way (`edited_at`, `ask.edited`; a request that names none keeps the summary it has), and the hand-back then sets `requested_version` to the current version, records the thread's newest reply as the one it answered (`asks.handed_back_reply_id`, migration 0064, a foreign key to `comments` from 0066) and appends `ask.handed_back`, which leaves `edited_at` and the question as they were, so an answer the human started before it is not refused `ASK_EDITED` and the card's edit history gains nothing. While it is `human` the call hands nothing back: the same summary, or none, writes nothing, and a different one is `409 APPROVAL_WAITS_ON_HUMAN`, naming the question the human is reading, since rewording it would rewrite that card with no turn of theirs and refuse an answer they had started. The route answers 201 when it wrote anything and 200 when it wrote nothing, with the document's `approval` as the call left it. `docs.OpenApprovalAsk` locks that one row, `docs.RewriteApprovalAsk` is the one rewording a version move and a new summary share, and `handBackApprovalAsk` (`api/reviews.go`) the one hand-back. The move is in the version's sole author when one exists and otherwise in settlement's actor, `{kind: "system", id: "document-settlement"}`. What resolving an ask stores is written in one place, `docs.WriteAskResolution`, which settlement's retraction of a removed ask block and the resolve route both call. A request while the latest version is approved returns the approval and opens nothing. Answering an approval ask (humans only; `Request changes` requires text) writes an `artifact_reviews` row pinned to the version it names, which is the latest settled version at answer time, and appends `artifact.approved` or `artifact.changes_requested` (`{artifact_id, name, version, actor, reason, ask_id}`) on the document's owner alongside `ask.answered`. `POST /api/v1/artifacts/{id}/reviews` `{state, reason?}` (humans only) writes the same review from the document header, pinned to the version settled when the request arrives, and answers the open approval ask when present so the review keeps its thread. Every document read carries `approval` (`draft | awaiting | approved | stale | changes_requested`, with `latest_version`, the latest review's `version/by/at/reason/ask_id`, and `requested_by` and `waiting_on` while awaiting, the request's turn by `waitingOnExpression`, so an agent whose own revision moved its request, which sends it no event, reads that the request waits on it); `stale` is derived from versions. Every approval ask names its document and no other ask names one: `asks_approval_kind_check` (migration 0053) refuses a row that pairs `kind` and `approval` otherwise. Legion's design gate consumes the approval events; it is the exception path, not an every-issue step.
- `POST /api/v1/issues` and `PATCH /api/v1/issues/{key}` accept up to 20 labels. Dispatch trims labels, preserves case, removes case-insensitive duplicates, and returns `400 LABELS_INPUT` for blank or over-40-character labels; every label update emits `issue.updated` with its labels. `GET /api/v1/issues?label=<label>` is repeatable, normalizes filter labels identically, and case-insensitively matches every supplied label.
- A reply to an open ask (`ask_id` set on `POST /api/v1/issues/{key}/comments` or `POST /api/v1/artifacts/{id}/comments`) records `turn` (`comments.turn`, migration 0028): who holds the turn after it. A human author's reply always stores `agent` whatever the request says; a session author's stores `human` unless the request says `turn: "agent"` (a progress note - the agent still owes the next move). A reply under an answered or resolved ask records no turn (nothing is waiting; a requested `turn` is ignored there, as a human's is). `turn` on a comment that is not an ask reply is `400 TURN_REQUIRES_ASK`; any value but `human`/`agent` is `400 INVALID_COMMENT`. Comment objects carry `turn` (null under a closed ask and off ask replies). The column's only constraint is `turn requires ask_id`, so a pre-0028 server still draining during a deploy inserts its ask replies with a null turn; every reader coalesces a null newest-reply turn to `human`.
- Every ask the API serves is read through one row shape: `docs.AskColumns` + `docs.ScanAsk` decode the ask row (the document settler uses the same pair for indexed blocks and anchor-refresh events), while `api/ask_rows.go` extends it with the block ask's document (`askRowColumns`) and, for reads, the newest comment in its thread (`askReadColumns`, a `lateral ... limit 1` join). `opened_event_id` is attached afterwards by `attachOpenedEventIDs`, one `events` query per read served by the partial index `events_ask_payload_id` (`store/migrations/0030_events_ask_payload_id.up.sql`; its `type in (...)` list mirrors that query and must change with it). `asks.options` is always a JSON array, never null.
- Every open ask read carries `waiting_on` (`human` | `agent`), and `waitingOnExpression` (`api/ask_rows.go`) is the rule's one home: the ask reads (`askReadColumns`, which alias it `waiting_on` so the Inbox order names that column and evaluates the rule once a row), `listOpenAsks`, a document's awaiting approval (`attachApprovals`), and the approval route's hand-back decision (`askWaitingOn`) all embed it. A reply learns its `ask_waiting_on` with no second statement: the comment write reads the ask it joins with the rule's moved-request arm, `approvalMoved` (`describeAsk`), under the owner row every version write and hand-back takes first, and once inserted the reply is the thread's newest, so `commentThreadTarget.waitingOnAfter` applies the rule to it in Go - `agent` for a moved request, otherwise the reply's own `turn`. An approval request whose `requested_version` is below its current `version` is waiting on its agent. One whose newest thread reply is the reply its last hand-back answered (`handed_back_reply_id`) is waiting on the human, and so is one nobody has replied to; otherwise the newest comment's `turn` decides. The newest reply is the one that committed last: `comments.created_at` defaults to `clock_timestamp()` (migration 0065), the moment its insert runs, and both comment inserts (`createCommentFor`, `replyComment`) leave it to the default and run once their transaction holds the owner row. The old default, `now()`, was the transaction's start, which sorted a reply that began first but waited for the row before one that committed while it waited; an older binary still serving during a rollout names no `created_at` either, so the default orders its replies too. The hand-back reads that reply under the same owner row, so a reply that inserts after the hand-back sorts after the reply it recorded and decides the turn, however early its own transaction began. It is on `GET /api/v1/inbox` rows, `GET /api/v1/asks/{id}`, `GET /api/v1/issues/{key}/asks`, `GET /api/v1/artifacts/{id}/asks`, and the issue detail's `open_asks`; closed asks and every `ask.*` event payload omit it. A `comment.created` or `comment.answered` payload that replies to an open ask carries the resulting `ask_waiting_on`, and every route that writes a comment (`POST /api/v1/issues/{key}/comments`, `POST /api/v1/artifacts/{id}/comments`, and the delivery callback `POST /api/v1/comments/{id}/reply`) answers the same `ask_waiting_on` beside the comment row (`commentWriteResponse`, `api/comment_projection.go`; `model.Comment` is the stored row and carries no derived turn), so a stream consumer or the writing agent reports the value every ask read has without a refetch. A replayed delivery callback, which writes nothing, answers the stored reply alone.
- Every ask read also carries `referenced_by_count`: how many `graph_edges` rows point at that ask, counted for the whole page in one grouped query (`attachAskBacklinkCounts`, `api/ask_rows.go`) rather than one request per card. It is on `GET /api/v1/inbox` rows, `GET /api/v1/asks/{id}`, `GET /api/v1/issues/{key}/asks`, `GET /api/v1/artifacts/{id}/asks`, and the issue detail's `open_asks`; mutation responses omit it, and so does an `ask.*` event off the live broker — `attachAskEventFields` (`api/events.go`) stamps the count only on replayed event reads, and nothing builds a card from an event payload. A card that needs the count reads one of the routes above. `GET /api/v1/issues/{key}` carries the issue's own `referenced_by_count` the same way, so the issue page renders its header count without a graph request; `GET /api/v1/artifacts/{id}` carries no backlinks at all — the graph is `GET /api/v1/references?to=` (or `GET /api/v1/artifacts/{id}/references` for agents). An ask's own `replies_to` edges — its clarification thread, which every ask surface already renders — are excluded from that count and from `?to=<ask>`; `?kind=replies_to` still returns them.
- A write that moves the reference graph names what it moved. `refs.ReplaceCounted` is the one way to index a body: it reconciles that source's edges (`replace`, unexported, so no producer can reconcile without holding what moved) and keeps the targets whose readers carry a batched count — an issue and an ask — which the event carries as `references_changed` (`[{kind, id, issue_key?, artifact_id?}]`), with `references_changed_truncated: true` past `refs.CountedChangeLimit`. A consumer refreshes exactly those rows (the ask's lists, its thread and the Inbox, the issue's detail) instead of sweeping every list; a write that cited nothing counted carries neither field. `grep -rn 'refs\.ReplaceCounted(' --include=*.go internal` lists every write that indexes a body, and the reconcile half is callable only from inside `refs`.
- Every payload family that can carry reference changes is built by a helper that takes them, so a producer that forgets does not compile: `artifact.*` through `api.artifactCreatedEventPayload` and `docs.ArtifactVersionEventPayload` (`writeVersionTx` returns `model.ReferenceChanges`, `SnapshotVersion` and `NamedVersion` return them on a `docs.VersionResult`, and the six producers — a settle, an upload, `POST /artifacts/{id}/versions`, `POST /artifacts/{id}/edits` (the route `dispatch_doc_edit` uses), accepting a suggestion, and an anchor snapshot on an ask or a comment — each pass what their write moved); `ask.*` through `model.NewAskEventPayload` / `model.NewAskEditEventPayload`; `message.*` through `messageReplyThread.payload`; `comment.*` through `s.commentEventPayload`; and `issue.*` through `model.NewIssueEventPayload`, which is how `POST /api/v1/issues` names what the spec it seeds cited. A transition that writes no text passes `model.ReferenceChanges{}` rather than omitting the argument. The guard is the required argument plus review, not the compiler alone: a struct literal (`model.AskEditEventPayload{Ask: …}`, as `outbox/publisher_test.go` builds) still compiles. Re-runnable inventory: `grep -rn 'NewAskEventPayload\|NewAskEditEventPayload\|NewIssueEventPayload\|commentEventPayload\|artifactCreatedEventPayload\|ArtifactVersionEventPayload\|thread.payload(' --include=*.go internal`.
- The reference grammar accepts any segment as an id, so `dispatch://CORE-1/ask/hello` parses and is stored like any other target. Every lookup that resolves a stored target skips what resolves to nothing rather than failing the write: issue and artifact targets compare text (`key = any($1)`, `project_key || '/' || id = any($1)`) and ask targets are filtered to syntactically valid uuids in Go before `id = any($1)`, which keeps the `asks` primary key on a query that runs inside every body-indexing transaction. A uuid cast in SQL would turn one malformed citation into a 500 on the write, and — because such rows are already stored — into a comment that can never be edited again.
- `GET /api/v1/inbox` rows carry the owning issue's nullable `priority` and `last_reply` (`{author, created_at}` of the newest comment with that `ask_id`, or null) beside `waiting_on`. It orders rows by the owning issue's priority first (`P0` on top, unset priorities last; a document ask has none), then by whose turn it is (`waiting_on: human` before `agent`), then by recency (newest reply, else the ask's creation), so a P0 ask an agent is still working on outranks a P2 ask nobody has answered; the SPA partitions by `waiting_on` and keeps this order inside each section. The client uses the same open-ask response to surface every `waiting_on: human` row under `Waiting on you` and its `Blocked on you` count; `waiting_on: agent` rows are `Waiting on agents`, whoever replied last. `GET /api/v1/issues/{key}` carries the same `last_reply` and `waiting_on` on every `open_asks` row (`issueOpenAsk`, `issue_queries.go`), so the issue header applies the inbox's whose-turn rule without the human-only inbox. A `comment.created` payload that replies to an ask carries `ask_state` (the ask's state at posting time) beside `ask_question`, so an agent can tell a clarification request on its open ask from discussion after the answer.
- `GET /api/v1/inbox?assignee=me|unassigned|<login>` (one value, combining with `project`) narrows the human-only inbox by the owning issue's assignee (`inboxAssigneeFilter`, `api/inbox.go`): `me` is the caller's lowercase login; a `<login>` is canonicalised (`strings.ToLower(strings.TrimSpace(...))`) before the allowlist check, so a shared `?assignee=Alice` link never 400s, while an unlisted login is `400 ASSIGNEE_NOT_ALLOWED`; `me` / `<login>` keep issue-owned asks with `i.assignee = $login`, `unassigned` keeps asks on unassigned issues **and every project-document ask** (a document has no assignee, so it is unassigned by definition); no value returns everything. This is the contract for shareable links and humans' scripts: the SPA fetches the unfiltered inbox once (the one `["inbox"]` query every surface shares) and partitions it client-side into **Mine** (rows whose `issue.assignee` equals the lowercased `/auth/whoami` login, plus an **Unassigned** band with an `Assign to me` PATCH on issue rows) and **Everyone**, defaulting to Mine and remembering the choice per login (`inbox.view`); a `?view=mine|everyone` URL wins over the remembered choice.
- `GET /api/v1/asks/open` takes exactly one scope selector. `?author_session=<session-id>` lists every active open ask authored by that session across open issues and unlinked project documents; `?project=<KEY>` lists every active open ask in that project whoever authored it, covering both ownership paths (issue-owned via `issues.project_key`, document-owned via `artifacts.project_key`, the same `coalesce` the inbox uses). Neither selector is `400 AUTHOR_SESSION_OR_PROJECT_REQUIRED`; both together are `400 AUTHOR_SESSION_OR_PROJECT_CONFLICT`. Either scope returns the same row shape: exact counts split by whose reply is next plus the full oldest-first inventory, with `session_id` echoing the selector (empty under project scope). `[&since=<RFC3339>]` reports `opened_since` over all asks in scope so a just-closed ask still disarms an automatic reminder.
- Event IDs are the SSE resume cursor. `events.Broker.Append` takes one global transaction-scoped advisory lock after it locks the event owner and before inserting, so IDs are allocated in commit order. A transaction that appends to multiple owners must call `LockOwners` with every owner before its first append; it locks them deterministically before any transaction can hold the global lock. This deliberately serializes concurrent event-producing transactions across every owner; do not bypass the broker with direct event inserts.
- Project creation, repository mappings, and architecture sources own project-scoped events (`project.created`, `project.updated`, `settings.repo_project.updated`, `settings.architecture_source.updated`, and the importer's `architecture.synced`/`architecture.sync_failed`, authored by the system actor `{kind: "system", id: "architecture-importer"}` and emitted only on state change: a new commit, a recovery, or an error whose text first appears or changes); per-user issue-state writes emit project-owned `user_state.updated` and do not advance an issue unread sequence. These events are retained on `notifications.dispatch.project.<PROJECT>.<type>` but never wake agents or take a direct agent/role route.
- The architecture importer (`internal/dispatch/architecture`, migration `0037`) reads every regular `*.md` (mode 100644/100755; symlinks and nested directories skipped) directly inside `.dispatch/architecture/` at the source branch's head commit with one subtree read (`GET /git/trees/{commit}:.dispatch/architecture`, never the recursive repository tree; GitHub's 404 is an empty model) and one blob read per file (an entry listed over 1 MiB is a typed `file too large` failure before any blob is fetched), validates the set whole (`Parse`: slug file names, valid UTF-8 without NUL, front matter via yaml.v3 with unknown keys rejected — the closer is a line that is exactly `---`, and an empty block is defaults — duplicate ids, unknown parent/depends_on, containment cycles, path hygiene), and projects it in one transaction: an immutable `architecture_snapshots` row per `(project, commit)`, delete+reinsert `components`/`component_depends`, and the source row's `last_sync_at`/`last_commit`/`last_tree_sha`/`last_error`. An invalid set only records `last_error` (capped at 4 KiB: the first problems and a count of the rest, the same text the `sync_failed` payload carries); the previous projection stays up. `graph_edges` gains the `part_of`/`depends_on` arms over node kind `component` (`<project>/<id>`), never rows in `refs`. Every sync runs under a two-minute deadline and, in-process, a per-project mutex; `project()` and `recordFailure()` open with `select … for update` on the source row, so two server processes never deadlock inside delete+reinsert, and a sync whose repo/branch changed while it was fetching writes nothing. A healthy source whose head is the recorded commit only refreshes `last_sync_at` (no fetch, no event); a moved head whose subtree sha equals `last_tree_sha` records the commit without re-fetching or re-projecting — so a caller looping the sync route is bounded to the head lookup. Triggers: a five-minute jittered ticker in `cmd/dispatch/main.go` (idle without App credentials), `POST /api/v1/projects/{key}/architecture-source/sync` (authAny; 200 with `last_error` on a recorded failure, 409 `SOURCE_ACCESS` for credential/branch problems, 404 without a source), and the `dispatch_architecture_sync` tool riding that route. `DELETE /api/v1/projects/{key}/architecture-source` also deletes the project's `components` and `component_depends` in the same transaction (the graph loses its component arms); snapshots remain as history. A sync that hits its own two-minute deadline while the caller is still alive is recorded on the row like any other failure (the record write runs on a cancel-free context); a caller that has gone away records nothing. The state-change decision behind both events is made from the row as read under its lock, so the second of two server processes that fetched the same head (or hit the same failure) stays silent. A `PUT` that re-points a source resets `last_tree_sha` with the other sync columns, so a byte-identical architecture directory under the new repository still re-projects once.
- Issue component attachment (`api/issue_components.go`, migration `0038`): `issue_components(issue_key pk, mode in (explicit, none), reason)` is an issue's own attachment and `issue_component_members(issue_key, project_key, component_id)` its explicit set; no row means inherit. Members reference `components` softly (no FK), so a re-import that retires a component leaves the link in place and every read reports the id under `unknown` instead of dropping it. `POST`/`PATCH /api/v1/issues[/{key}]` take `components` as a tri-state raw field like `parent`: `null`/`{mode: inherit}` deletes the row, `explicit` needs one to fifty slug ids that are components of the issue's project and not `external` (`400 COMPONENTS_INPUT` names each unknown, retired, external, or other-project id), `none` needs a non-blank `reason`; the closed-issue gate exempts `components` like `rank`, and a write bumps `issues.updated_at` and rides the ordinary `issue.updated` event with the whole issue. Resolution is on read, never stored: `issueComponentsLateral` is one `left join lateral` (a depth-capped `union` walk up `parent_key` to the nearest row of either mode — `none` is inherited exactly like `explicit` — splitting members into live and retired ids) that `loadIssue`, both list queries, and the tree route share, so every `Issue`/`IssueSummary` carries `components {mode, ids, unknown, reason, inherited_from}` from the same query. `GET /api/v1/projects/{key}/architecture` (`api/architecture_tree.go`, authAny, `404 SOURCE_NOT_FOUND`) computes the counting rule once: every project issue's effective set joined to the component containment closure (recursive over `components.parent`, depth-capped), one row per (issue, component) with the strongest way it qualified (`direct` > `inherited` > `contained` with `via`), aggregated in Go into `done`/`total` (distinct issues, icebox and closed included) and `own_*` (those naming the component itself), plus the `unassigned` (no row anywhere), `not_architectural` (a `none` row, own or inherited), and `retired_links` (non-empty `unknown`) lists and `totals` (`components_without_work` counts non-external components with `total == 0`). `graph_edges` gains the `affects` arm (`issue` → `component` per explicit member, `created_at` the row's `updated_at`; inherited attachments are not edges), `refs.Kinds` learns `part_of`/`depends_on`/`affects`, `refs.resolveNodes` loads `component` nodes from the current model (a retired id resolves to nothing and its edges are omitted), and `text.Extract` parses `dispatch://<PROJECT>/component/<id>` so a mention of a component is an ordinary `mentions` edge.
- Per-user agent state is its own tables and route pair, not a key in `GET /api/v1/me/state` (that map is keyed by issue). `PUT /api/v1/me/agents/{session_id}/state` (human-only) takes `{cleared_before?, read_through?, read_replies?}` (the first two RFC3339; at least one): `cleared_before` upserts the viewer's Clear into `user_agent_state(login, session_id, cleared_before)` (`store/migrations/0033_user_agent_state.up.sql`), replacing the previous one, and `read_through` upserts their read mark into `user_agent_read(login, session_id, read_through)` (`0051_user_agent_read.up.sql`), which only moves forward so a tab that read less a moment ago cannot make a reply unread again, keyed on `canonicalLogin(actor.ID)` as `user_ask_snooze` is. `read_replies`, the ids of messages the path's session wrote, adds one row per reply to `user_agent_reply_read(login, session_id, reply_id)` (`0067_user_agent_reply_read.up.sql`, keyed on the canonical login too, primary key `(login, reply_id)`), which reads those replies and no other: a view that shows only some of a session's replies (the broadcast page, each recipient's reply to that broadcast) writes it, because moving the read mark through them would also read the session's older reply to another message, which the viewer never saw. A reply the session's read mark has already passed gets no row, so a revisit of replies the mark covers writes nothing. A `read_through` deletes, in the same transaction, that viewer's rows for that session whose replies it now reaches, which the mark already reads (`user_agent_reply_read_session`, on `(login, session_id)`, serves the delete), so a row lives only until a read mark, which only moves forward, passes its reply; a session the viewer reads only on broadcast pages keeps its rows, since nothing else records those reads. An id is taken in any form `uuid.Parse` reads (a `urn:uuid:` prefix, braces, upper case) and sent to Postgres in the canonical form, which Postgres takes; one `uuid.Parse` cannot read, or one that is not a message the path's session wrote, is `400 INVALID_STATE`, and nothing the request names is written. `0051` also inserts one read mark, at the migration's own time, for every (human, session) that already had an issue-less direct message, so the deploy does not turn every reply ever stored into an unread one. The read mark is a table of its own because a viewer who has only read a conversation has no Clear, and `user_agent_state.cleared_before` stays `not null` for a Dispatch that predates the read mark and scans it as a timestamp. No field given, a malformed value, or a cutoff more than a minute ahead of the server clock (`agentStateCutoffSkew`, the allowance for a fast browser clock) is `400 INVALID_STATE`; a value the skew admits is stored as `least(value, now())`, so a reply that lands while a fast browser's mark is still ahead of the server counts; the PUT answers the session's whole state. `GET /api/v1/me/agents/state` returns `{[session_id]: {cleared_before?, read_through?, unread_replies}}` for the caller: `unread_replies` counts the rows of `unreadDirectRepliesCTE` (`api/unread_replies.go`, the one definition the conversation window's unread term reads too): the session's replies anywhere under a direct message this viewer sent it (an issue-less message targeted at it, a broadcast's copy included, whose author matches the viewer's canonical login) that are newer than both the read mark and the Clear and that the viewer has not read by id, and a session with unread replies appears even with no stored row. The PUT touches no message and appends one `user_agent_state.updated` event (`{login, session_id}`, owned by the session like its issue-less messages, so the outbox publishes it nowhere and it never notifies), which the viewer's other open tabs and devices refetch their state on; a PUT that names only replies already read by id or passed by the read mark changes nothing and appends none, since a broadcast page sends its replies again on every visit while the session has an unread reply elsewhere. A reply the Clear hides but the read mark has not passed is neither, though `unread_replies` leaves it out: the Clear can move back, so the first PUT naming that reply writes its row and appends the event. The Clear is a view preference the Agents page applies client-side (an exchange is hidden when its newest message is at or before the cutoff), so a reply that lands after the Clear still surfaces. The SPA refetches the state on an issue-less `message.answered`, the one event every reply row is written with, and on this event for its own login; how it shows and marks unread replies is `packages/dispatch/AGENTS.md`'s (`features/agents/`).
- A human's snooze on one inbox row is its own table and route pair, like per-user agent state. `PUT /api/v1/me/asks/{id}/snooze` (human-only, because the inbox is) takes `{snoozed_until: <RFC3339>}` and upserts `user_ask_snooze(login, ask_id, snoozed_until)`, keyed on `canonicalLogin(actor.ID)` rather than the actor id — a human's actor id is the login as their identity source spells it (`CookieIdentity` returns GitHub's display casing, `HeaderIdentity` the header verbatim; both lowercase only to check the allowlist), so the raw id gives one person two snooze sets and orphans their rows when GitHub's casing changes. That makes the `me/*` family inconsistent, deliberately and visibly: `user_ask_snooze` and `user_agent_read` (`0051`) key on the canonical login, while `user_issue_state` (migration `0001`) and `user_agent_state` (`0033`) key on the raw `actor.ID`, and `inbox.go` now binds one form twice — `canonicalLogin` for the assignee filter's `$3` and for the snooze join's `$4` — while those two tables are read elsewhere under the raw id. The two older tables carry the same latent split; it is not introduced here and is not fixed here, because normalising them needs a backfill of rows that already exist. `user_ask_snooze` has none to back-fill: the table is created by `0048` and its only writers ship in the same change, so no deployed schema ever carried a version of it written under a raw login — (`store/migrations/0048_user_ask_snooze.up.sql`); a non-uuid id is `400 ASK_ID_INPUT`, an unknown ask is `404 NOT_FOUND` (the insert selects the ask rather than naming its id, so the foreign key never surfaces as a 500), and a missing, malformed, or already-past `snoozed_until` is `400 SNOOZE_INPUT`; the bound allows no skew, and the SPA's presets keep clear of it (the one that can land near midnight falls through to the next morning rather than naming a moment inside the request's own latency). `DELETE /api/v1/me/asks/{id}/snooze` removes it and answers `204` whether or not a row was there. Neither write touches the ask, appends an event, or reaches any other login: a snooze is one viewer's view state. Keyed on the ask, not its issue, so a document ask snoozes exactly like an issue ask. `GET /api/v1/inbox` carries the caller's own `snoozed_until` (null when they have not snoozed the row) through a left join on that table and **never filters on it** — the row is listed either way and nothing sweeps the table, so a snooze whose moment has passed reads back as the ordinary row it is. The SPA folds a row whose moment is still ahead into a collapsed `Later` band, ahead of whose turn it is: an agent replying to a deferred ask hands the turn back without bringing the row back (`sectionOf` and `waitingOnYou`, `packages/dispatch/web/src/features/inbox/`).
- `GET /api/v1/issues/{key}/subscribers` and `GET /api/v1/artifacts/{id}/subscribers` (human-only) list the sessions whose persisted Envoy interests match that issue's or unlinked document's topic family, merging `GET /v1/interests/` with `GET /v1/sessions` for live status and title. `DELETE .../subscribers/{session_id}` removes matching topics with `POST /v1/interests/unsubscribe`; it first commits `subscription.remove_requested` with `pending: true`, which the outbox does not publish. After idempotent listener removal succeeds, Dispatch appends `subscription.removed` with the `request_event_id` it settles and routes that notice directly to the unsubscribed session's `notifications.agent.<session_id>` topic in addition to the owner topic.
- The Dispatch API describes itself. `internal/dispatch/api/routes_table.go` is the one list of `/api/v1` routes (`apiRoute{Method, Pattern, Auth, Description, Handler}`, `Auth` one of `public`, `any`, `human`, `bearer`); `Register` mounts that table and public `GET /api/v1` serves it as `{routes: [{method, path, auth, description}], docs: "skills/dispatch/SKILL.md"}` sorted by path then method. A new route is a new row (and a bumped pin in `routes_table_test.go`), never a `mux.HandleFunc` line. An unknown path under `/api`, `/v1`, `/auth`, `/ws`, or `/healthz` is `404 {"code":"NOT_FOUND","error":"no route for GET /v1/issues","hint":"GET /api/v1 lists every route"}` from `routes/router.go`, decided before any dashboard lookup, so an API caller never receives the SPA shell. `GET /api/v1/agents` is readable by any authenticated caller (a session picks a message target by the `capabilities` it advertises); the issue-less `POST /api/v1/agents/{session_id}/messages` and `GET /api/v1/agents/{session_id}/messages` stay human-only.
- Every session that writes to an ask follows it: `asks.FollowAuthor` runs at all three ask insert sites (`POST .../asks`, approval requests, ask blocks indexed from documents) and on every ask reply, whether it arrives through `POST .../comments` or through the delivery callback `POST /api/v1/comments/{id}/reply`, so no insert path can miss it; `store/migrations/0029_ask_followers.up.sql` backfills existing asks and replies. `GET /api/v1/asks/{id}` returns `followers` (`{session_id, since}`, oldest first); `GET /api/v1/asks/{id}/followers` (any authenticated actor) returns the same list. `PUT` / `DELETE /api/v1/asks/{id}/followers/{session_id}` add or remove one follower and append `ask.follower_added` / `ask.follower_removed` (`{ask_id, session_id, by}`): a bearer sends `{ "actor": { "kind": "session", "id": "<own id>" } }` in the body on both verbs and may act only when the path session equals it (`403 FOLLOWER_FORBIDDEN`); a human (cookie identity) sends no body and may add or remove any session. A non-UUID ask id is `400 ASK_ID_INPUT`; removing a session that does not follow is `404 FOLLOWER_NOT_FOUND`; a repeated `PUT` is a `204` no-op with no event.
- A reply joins its thread the same way on both write paths. `threadHeadOf` (`api/comment_create.go`) climbs `reply_to` from an already-locked comment to the head of its thread and, when that head is an ask, the reply stores the ask's `ask_id` with `reply_to` null and the reply's `turn`; `POST .../comments` reaches it through `normalizeCommentThreadTarget`, which validates the request's `reply_to` first, and `POST /api/v1/comments/{id}/reply` calls it directly with the comment it already holds locked, so a session answering a mention inside an ask's thread lands in that thread instead of in a `reply_to` chain no ask read selects (`store/migrations/0043_comment_reply_ask_id.up.sql` moves the rows written before that, and gives each the turn 0028 gave every other ask reply). `comment.delivery` payloads carry the `ask_id` of the comment they report on, so a receipt names the thread it changed. Neither event is published to the ask's followers: `publishFollowerRoutes` routes `ask.answered`, `ask.edited`, `ask.handed_back`, `ask.resolved` and `comment.created`/`resolved`/`reopened`/`edited` with an `ask_id`, and no other type.
- Keep Envoy API-level with OpenCode. Do not add DB introspection or OpenCode-specific hidden coupling unless there is no API path.
- Dispatch caps (`CAP_EXCEEDED` 400) read `<field> is N characters over the M-character limit (L/M)` (`capExceededError`, UTF-16 units) or `<field> is N over the M-item limit (C/M)` for counts; `GET /asks/{id}`, `/comments/{id}`, and `/issues/{key}/messages/{id}` answer a non-uuid id with 400 `<KIND>_ID_INPUT` (`requireUUIDPath`); a document-edit quote miss (`TARGET_NOT_FOUND`) names the three nearest blocks and a `# Title` quote selects a heading. The outbox `payload_summary` of `ask.answered` is the answer rendering (`<selected> - <text>`), not the question.
- Listener message APIs preserve the human `message` as a one-line `payload_summary` of at most 160 characters; when it differs, the complete message is `payload`. A supplied `payload` for `/v1/messages/publish` wins over the derived value.
- Ghost Wispr only publishes `session_started`, `session_ended`, and `summary_ready`; other verified events should return 200, log the skip, and not publish.
- `ENVOY_GHOSTWISPR_SIGNING_SECRET` is optional for trusted Ghost Wispr deployments; when unset, skip signature verification explicitly rather than half-verifying missing headers.
- GitHub mention routing is additive: matching comments publish to both `.comment` and `.mention` topics.
- Slack topics must use the real Slack `team_id`, not a workspace slug.
- NATS peer storage uses named Docker volumes, not repo-path bind mounts.
- Role lanes use core NATS, not JetStream: the listener queue subscriber resolves the live holder at delivery time, then makes a receipt-backed request to that holder's agent subject (`bus.Client.RequestCoreTo`). The agent pump returns an empty receipt after accepting the envelope. No receipt within two seconds from a registered, live holder is `receipt_timeout` (the message was forwarded and not acknowledged; the Legion daemon treats it as delivered to a live process) — keyed on `bus.ErrReceiptTimeout`, which `RequestCoreTo` returns only after the publish and the flush both succeeded, the server was shown to have accepted the forward, and the receipt wait ran out; the flush is bounded by the same two-second window, and a forward whose window ends while NATS is reconnecting, or a flush that fails or times out (a stalled connection still buffering the forward), is the client's own error, so it is `delivery_failed`, never `receipt_timeout`. So is a forward the server denies under the listener's grant (`bus.ErrPublishDenied`, returned as soon as the flush answers), and one the server cannot be shown to have accepted when no receipt came; `bus.confirmPublished` holds how either is told apart from an accepted forward. `delivery_failed` is a claim whose message is not known to have reached the holder (holder lookup failed, holder stale, the publish or flush failed or was denied); `no_holder` is no claim at all. Every reason emits an exception; the attempt cache holds an entry only while a forward is in flight and both forward failures roll it back, while the dedupe cache records a forward only when its receipt arrived — so a publish that re-uses a `dedupe_key` after a `receipt_timeout` is forwarded again, while one after a delivered forward is skipped. Do not add durable role consumers or retry transit for role messages.
- Role ownership is durable in the `envoy_roles` JetStream KV bucket. Each role key records `holder_session_id`, `claimed_at`, and `previous_session_id`; listener restart restores the claim from that record, but routes only while the holder is present in the `envoy_sessions` registry. Reaping stale interests never releases a role; a restored absent holder gets one registry TTL to re-register, then loses its claim atomically on the role reaper or next resolution, while the first core role delivery still emits its normal delivery exception. A listener reads a claim from the role bucket itself but a holder's liveness from its cache of `envoy_sessions`, which trails that bucket, so a holder another listener registered and gave the role a moment ago is in both buckets before it is in this cache; during a rolling deploy the old task resolves every claim the replacement accepts. Nothing is taken from a holder on the cache's word: before a lookup or a role delivery releases a claim as lapsed, the role reaper ends one, or a soft claim supersedes its holder, a holder the cache misses (or, for a role delivery, holds with a heartbeat older than `session.ClaimStaleAfter`) is read from the bucket itself (`roleHolderSession`, `session.SessionRegistry.Refresh`). A holder no session can register as (a key `bus.KeyValue` refuses, or one outside nats.go's key alphabet such as `ses:bad`) counts as gone. A read that does not answer within `roleHolderReadTimeout` (2 s, inside the 10 s HTTP write timeout) releases nothing: a lookup answers 500, a soft claim 503, a role delivery reports `delivery_failed`, and the reaper keeps the claim and logs a WARN. That read is a direct get, so the listener's NATS user needs publish on `$JS.API.DIRECT.GET.KV_envoy_sessions.>`.
- `store.Open` snapshots the revision of every stored claim (`roleRevisions`, `internal/store/kv.go`) so a restored claim keeps its grace only while the bucket still holds the revision the restart read. It takes that snapshot from one watch over the bucket's existing keys, as the interest, session and CI caches read theirs (`internal/kvwatch`), so listener readiness costs one pass over the bucket rather than a round trip per claim (LEGION-360). A scan that does not reach the end of the bucket fails the start rather than restoring part of it: nats.go ends one early both by closing the updates channel and, on its own idle timeout, by sending the same nil marker a complete scan ends with, so the snapshot reads `Error()` at the marker. The timer belongs to the watch, not to this reader — `nats.KeyValue.Keys()` has it as well — so a build that lists the keys and reads each one back restores a partial snapshot just the same over a link that goes quiet for the JetStream `MaxWait` and then recovers. Unlike those three, this one is a snapshot and not a live cache — the grace window is anchored to the moment `Open` returns — so nothing rewatches it. The snapshot names no key, so it also carries a claim stored under a key this build cannot read (`bus.ErrRefused`); that claim still gets no grace, because `ReleaseExpiredRoleClaim` reads the claim itself first and cannot, and the role reaper deletes it. A start logs the snapshot it restored as one INFO line, `restored role claims`, with `restored` (claims that kept their grace) and `delete_markers` (tombstones streamed past) as disjoint fields; a single total of the two reads as claims the restart failed to restore. `internal/store` writes it, and both reaper cycles, through the logger the listener passes to `store.Open` (`store.WithLogger`), so they are JSON records with `machine_id`. The listener must not reach that by `slog.SetDefault`: that also routes `internal/bus` and the stdlib `log` package into the JSON handler, and the deployed CloudWatch metric filters for publish failures, webhook refusals and dropped stream subjects are space-delimited text patterns anchored on that package's date and time prefix, so three alarms would stop matching without anything failing.
- The bucket's subject count is not its claim count, and sizing a restart from `nats stream info KV_envoy_roles` overstates it. A limits-retention KV keeps a delete marker on the subject of every key ever deleted, and both `nats kv ls` and `nats.KeyValue.Keys()` hide them, so most of a long-lived bucket's subjects are markers of claims that ended. A marker is not a claim and gets no grace; it costs one header-only message in the snapshot above, so readiness grows with the subject count at the link's bandwidth rather than by a round trip each. Nothing expires markers, and a KV `MaxAge` would expire claims with them; `nats kv compact envoy_roles` (`KeyValue.PurgeDeletes`) purges marker subjects alone and leaves live claims at their revisions, keeping markers under 30 minutes old.
- A failed control delivery or a terminal capability refusal during generic fanout emits `notifications.envoy.exceptions.<original-topic>`. Control exceptions keep their ordinary transport; a generic fanout refusal uses core NATS because the fanout API accepts arbitrary non-control topics, so retaining every possible exception subject would also retain role exception lanes. The payload preserves `original_topic`, `event_id`, `reason` (one of `no_holder`, `delivery_failed`, `receipt_timeout`), `recipient_session` when a recipient is known (the receiving session, not `source_session`; omitted rather than empty when unknown), `payload_summary`, the original machine `payload`, `dedupe_key`, `source`, and `source_session`; the exception lane is not recursively exceptional. Each refusal records its recipient before publishing its exception, so an identical redelivery emits at most one exception during the attempt-cache window and never NAKs the original envelope. An API publish to an unheld role is rejected synchronously with 404 instead.
- **Source-specific vs generic ingestion**: Envoy has two ingestion paths: listener-hosted webhook handlers behind the listener's starting gate (`internal/webhook/{github,slack,ghostwispr}.go`, `startingGate` in `cmd/listener/main.go`) and the generic MCP bridge (`cmd/mcp/`). The MCP bridge connects to any MCP server that publishes resources, so it's the low-maintenance default for new sources. Building source-specific webhook logic adds maintenance burden — consider whether the cost justifies the benefit over the generic MCP bridge before adding custom source-specific logic to Envoy. When using the MCP bridge, Envoy should stay naive about the message content — the MCP server owns the domain logic.

## Security

Dispatch treats an agent endpoint and bearer token as one trust-bound configuration: a repository `dispatch.serverUrl` can use only the token in that same repository file, while explicit environment configuration supplies both. The deployed server's `DISPATCH_SERVER_URL` is separate: it overrides the merged `dispatch.serverUrl`, must be an absolute `http` or `https` URL with no path, and is the exact GitHub OAuth callback origin. `NATS_URLS` likewise overrides merged `natsUrls` for the server. A GitHub login the OAuth callback exchanges but `DISPATCH_ALLOWED_LOGINS` does not list is logged (`dispatch: login not allowed login=<login>`) and answered with a 403 HTML page naming that login and linking back to `/auth/start`, so an operator can find who to add; the JSON `LOGIN_NOT_ALLOWED` stays on the API paths. Browser sessions carry a server-side generation that logout advances, and unsafe cookie-authenticated requests must prove the configured same origin; bearer automation remains separate. JSON decoding is limited to 1 MiB, multipart uploads retain their explicit 26 MiB limit, and the GitHub proxy has the same bounded request buffer. The event outbox retries each required issue, route, and author destination with exponential backoff, so a failed or poison delivery cannot be marked complete or starve later notifications. A destination NATS denies holds back none of the event's others; a connection or store failure ends the attempt at that destination, and the event is retried (`publish`). A core-NATS destination (a role lane) counts as published only once `bus.Client.PublishCoreTo` has shown the server accepted it, at the cost of one flush round trip (`bus.confirmPublished` holds how): one the server denies under Dispatch's grant (`bus.ErrPublishDenied`, naming the subject) or cannot be shown to have accepted is left unrecorded and retried, rather than recorded in `published_destinations` for a message nobody received. Each retry line (`dispatch outbox: publish event`) names the destination by label and target (`publish owner topic "<subject>"` for the event's own topic, `publish route "role:reviewer"` or `publish route "session:<id>"` for the issue's route, `publish author route to "<session>"`, `publish follower route to "<session>"`, `publish claim change to "<session>"`); a core denial, which only a `role:` route can be since the role lanes alone take core transport (`usesCoreTransport`), adds the subject and the server's violation. The listener's own core publishes (a role topic through `/v1/messages/publish`, a fanout exception) fail the same way, the API publish answering 500 with the violation. A destination NATS refuses (`bus.ErrRefused`: an event past the server's max payload, or a subject past NATS's limit or holding whitespace or an empty token, which an unbounded document slug or a bearer's session id such as `sess..x` can make) is refused the same way on every retry, so it is logged once (`dispatch outbox: destination refused`, naming the event and topic) and counted done, and the event goes on to its other destinations. The listener holds one line for both of the bearer kinds `/v1` accepts: the shared-token compare stays constant time and is skipped entirely when no shared token is configured, so no request authenticates against an empty one; a JWT-shaped bearer the verifier rejects is answered 401 and never falls back to the shared token or any other path; and the 401 is the same `unauthorized` in every case, so a rejected caller learns neither the reason class nor which credentials the listener is configured for.

## Operational notes

- Health endpoints reflect dependency health, not just process liveness. `/healthz` returns `degraded` for transient JetStream/KV probe failures and `unhealthy` for NATS loss, a stopped interest, session or CI KV watcher, or a missing durable consumer. A listener that mounts no GitHub webhook route keeps no CI cache, and its healthy answer carries `"ci_cache": "not_applicable"`.
- NATS reconnects indefinitely, in place: the bus starts from `nats.GetDefaultOptions` (reconnect, client pings, drain and flusher timeouts), so the connection object and every JetStream or KV handle taken from it survive a server restart. A publish while reconnecting waits for the reconnect within its own deadline (5 s for a publish, 2 s for a role-lane forward), and reconnect attempts run every second. The reconnect buffer is off, so a publish that fails was not sent. A subscription nats.go re-sent on the reconnected connection is kept as it is; one on a replaced connection is bound again. Every reconnect recreates the interest, session and CI KV watchers on the reconnected connection, because a server restart loses their ordered consumers and nats.go would replace them only after missed heartbeats, and a connection the bus dials to replace a closed one moves them onto itself. After each of those connection events a run of the reconnect hooks that do this begins that covers it, whatever re-subscribing returned, since a listener waiting out the task that holds its durable has that durable's re-subscribe refused by design while `/v1` and the role lane already read the caches. A run covers every event counted before it began, so an event that lands between runs gets a run of its own and a burst of events that land before one begins gets one. Runs take turns (`bus.Client.rewatch`), so a recovery retrying that re-subscribe waits for a run in progress rather than starting a second beside it. A run that fails is run again by a recovery, the only way an event gets a second run: the failure starts one, and when one is already ending the failure finds it running and starts none, so a recovery looks again once it has cleared its flag. Re-subscribing holds no lock across a bind's request, so a reconnect that lands while a bind is in flight, such as the listener's bind attempt, whose lookup the reconnect can lose for JetStream to wait out for 10 s, restores and runs the hooks without waiting for it: a registration has one bind at a time, so the restore leaves that registration to its bind, and the recovery the reconnect starts binds it should that bind fail. Nothing has failed there, so the restore logs it at INFO, `envoy nats resubscribe left to the bind in flight` with the `subject`, and `envoy nats resubscribe failed` and `envoy nats recovery resubscribe failed` stay ERRORs for binds that failed. While NATS is connected, the self-health monitor also rebuilds those watchers and a missing durable consumer when a probe finds a stopped watcher, a closed KV handle or a lost consumer. All three watchers share one lifecycle (`internal/kvwatch`), and the listener keeps its caches as one list (`listenerCaches`, which holds the CI cache only on a listener that mounts the GitHub webhook route), which rewatch, self-health, `/healthz` and shutdown each loop over. The first start runs in the background. When it fails with no watcher current, it records the failure and releases the cache's readiness; when a Rewatch's watcher is current, it does neither, and that watcher releases readiness once it has delivered every existing key. A rewatch opens the bucket on the connection it is given, so a store bound to a replaced connection moves to the new one. A rewatch onto a bucket whose stream was deleted and created again empties the cache and its revision fence before the new watcher fills them. A bucket recreated under a watcher that did not end (nats.go's ordered consumer can reset onto the new stream without delivering its first revisions) is caught by each store's `Ping`, which the self-health probe runs: it records a terminal watcher error, so the next tick rebuilds the watcher. A watch whose stream is older than a running, healthy watcher's (it read the bucket before a newer watch switched to a recreated one) is discarded, and read and dropped until it ends; against an ended or flagged watcher it installs as usual, since a restored or clock-stepped bucket can have an older creation time. Only the current watcher releases readiness, at the end of its scan or when it ends on its own; a replaced watcher's end-of-scan releases nothing. An entry from a watcher already replaced is dropped. A watcher that ends on its own records the terminal error `/healthz` and self-health read. A stopped watcher arms nothing, and one armed while the stop ran is read and dropped until the drain ends it. A watcher reporting `consumer not active` during the gap is logged at WARN, and so is a shutdown drain that finds a watcher's consumer already gone (`consumer not found`): the drain deletes each consumer nats.go created as it ends that subscription, and the ordered consumer of a watcher the last reconnect had not yet replaced can already be gone (a restart loses it outright, since it is kept in memory; a disconnect longer than its inactive threshold lets the server delete it). At the pinned nats.go that delete is the only report of the bare error, so the bus warns on the bare error alone; a nats.go bump re-checks that. The one other report of `consumer not found`, an ordered consumer nats.go failed to recreate, wraps the error and stays an ERROR. Every other async error is an ERROR.
- Every cache warm-up logs one line when its initial scan ends — `<cache> cache warm-up` with the bucket, `elapsed_ms`, `entries`, `delete_markers` and `outcome` — at INFO when the scan delivered every existing key and at WARN when nats.go's idle timer gave up on it (`outcome: "timed out"`, `internal/kvwatch`). The two counts are disjoint: `entries` is the live keys the scan delivered, `delete_markers` what it streamed past to find them; a cache can hold fewer keys than `entries`, since it evicts a value it cannot decode. The line, like every line a cache watcher writes (a failed first start, a recreated bucket, its own end), goes through the logger its store hands it (`store.WithLogger`, `session.WithSessionLogger`, and the CI store's own), so in the listener it is a JSON record with `machine_id`, as `internal/store`'s lines are. A timed-out scan is logged and never recorded as the watcher's error, because the watcher is still running and its cache still follows the bucket: `Err`, `Ping`, `/healthz` and the self-health rebuild must keep meaning "this watcher is dead". Reading the timeout consumes nats.go's single buffered error, so a watcher that later ends after a timed-out scan records `<cache> watcher stopped` rather than the timeout; `internal/kvwatch` reads that error in one place (`idleTimeout`), for the warm-up, a watcher's terminal error and the one-shot `ScanExistingKeys`, which refuses a scan the timer ended.
- Each listener collects the interest bucket's delete markers, because nothing else does: every delete path leaves one (the reaper for each dead session, an unsubscribe-all, the admin delete), the bucket has no `MaxAge` and keeps one message per subject, and each marker is replayed by every restart's cache warm-up before that listener serves, so uncollected markers grow every restart's time with no deliveries (LEGION-374). A pass reads the stream (read 1), runs one MetaOnly scan of the bucket, takes the **floor** = the lowest revision that scan delivered as a PUT, reads the stream again (read 2), and sends one unfiltered `STREAM.PURGE` of everything below the floor. The floor comes from the stream, never from the cache: it is a sequence that scan saw, every live key's latest message is that PUT or a later write with a higher sequence, no value is decoded on the way (so a live key this build cannot decode, which the cache evicts while its message stays in the stream, is protected), and a write after the scan is given a higher sequence and survives. It refuses to purge at DEBUG when there is nothing below the floor or no live key at all, and at WARN when a read of the stream failed, the scan did not complete, the stream was replaced, the sequence space moved backward, or the floor is above the stream's last sequence. Every pass logs one line, `interest markers collected` or `interest marker collection refused` with its `reason`, carrying every stream value the decision used — `read1_*` and `read2_*` (both reads come before the purge), `floor`, `live`, `delete_markers` — plus, when it purged, `purged`, `purged_first_seq` and `purged_msgs` from a read after the purge; a read the pass never took is absent from the line, so an unexpected purge or refusal is diagnosable from the line alone. `purged` is the drop in the stream's message count between read 2 and that read, so it is approximate both ways: a put in between under-reports it, and a peer listener's purge in between is counted by both passes, so a sum of `purged` across the fleet overstates what was removed. A purge whose count read fails still logs `interest markers collected`, at WARN and without the `purged` fields. A bucket with no live PUT is never collected, on purpose: a fleet-wide outage leaves every marker in place, which is safe. The creation time bounds the stream's identity and the sequences bound its space, and neither alone bounds both: a bucket deleted and created again has a new creation time but, when the original's first sequence was still 1, need not lower any sequence, and a JetStream restore keeps the snapshot's creation time and shows only as a sequence space that moved backward. A creation time re-stamped with no replacement (an in-place config update plus a restart, at 2.10) refuses one pass, which costs five minutes. A floor above `LastSeq` is the one value that makes the server's unfiltered purge compact the whole stream. One window cannot be guarded: `STREAM.PURGE` at 2.10 takes no expected-stream precondition, so a bucket deleted and created again between read 2 and the purge cannot be refused — that window is one round trip, which is why read 2 is taken immediately before the purge. Every listener runs a pass after its interest cache's first warm-up and then on the reapers' five-minute cadence; the purge is idempotent (a repeat purges 0), so there is no lock and no leader. The one capability it adds is publish on `$JS.API.STREAM.PURGE.KV_envoy_interests`: a NATS user without it leaves every marker where it was, logs `interest marker collection could not purge the bucket` once per process (the connection's own `envoy nats async error` names the permissions violation), and keeps serving.
- Only a terminal failure that remains after three consecutive recovery intervals self-terminates the listener. A rebuild that reports success is probed at once, and a healthy probe resets the count, so separate faults that each rebuild repairs (a bucket deleted and recreated, then another, then the durable) never add up to a restart, unless the probe right after a rebuild also fails: that failure, transient or terminal, keeps the count, and the terminal line names its error. A listener bucket deleted and not recreated (the interest, session, CI or role bucket) fails every rebuild, because a rebuild opens a bucket and never creates one, so it ends in a restart, and the next start creates the bucket again; a missing bucket counts as terminal, so this holds for the role bucket too, which keeps no cache watcher: `Rewatch` reopens its handle all the same, and its only watch is the one-shot revision snapshot `store.Open` takes. Shutdown stops HTTP first (up to ten seconds), waits within that window for a self-health rebuild still running, retires the interest, session and CI KV watchers (`StopWatch`, final and without a server request, so a reconnect hook still running cannot arm one), and drains NATS through `bus.Client.Drain`. The drain stops the client first, so it never reconnects, re-subscribes or registers a subscription, and lets deliveries already in their handlers finish, role-lane forwards included. It does not wait for a bind in flight: a subscription such a bind makes after the stop is drained with the rest, so the messages the server already routed to it reach its handler rather than being discarded, and the close ends a bind still in flight. It is bounded to ten seconds, and a connection that is reconnecting is closed at once. It logs completion and exits non-zero so Docker's restart policy can restore it. A runtime must therefore allow about twenty seconds after SIGTERM: the compose file sets `stop_grace_period: 30s`, and ECS's default `stopTimeout` is 30 seconds.
- The listener refuses to bind its durable (`listener-<machine id>`) when the durable's idle heartbeat or ack policy differs from the listener's consumer policy (no heartbeat, explicit acks), because NATS cannot change either in place. It checks the durable right after the NATS connect, before the cache warm-ups, logs `subscribe refused, shutting down`, naming the durable and every such setting it carries, and exits 1 at once rather than retrying a bind that cannot succeed. Deleting the durable lets the next start recreate it at deliver policy `all`, which replays every message the stream retains (72 hours). To keep its cursor instead, recreate it from its own config at one past its ack floor, while no listener runs for that machine:

  ```bash
  d=listener-<machine id>
  nats consumer info ENVOY_NOTIFICATIONS "$d" --json > "$d.json"
  jq '(.ack_floor.stream_seq + 1) as $next | .config | del(.idle_heartbeat, .flow_control) | .ack_policy = "explicit" | .deliver_policy = "by_start_sequence" | .opt_start_seq = $next' "$d.json" > "$d.recreate.json"
  nats consumer rm ENVOY_NOTIFICATIONS "$d" -f
  nats consumer add ENVOY_NOTIFICATIONS --config "$d.recreate.json"
  ```

  The listener's next start stamps the rest of its policy onto the recreated durable and binds it. A message past the ack floor that was already acknowledged out of order is delivered again.
- During a rolling deploy the replacement serves `/v1` and its role lane while the old task still holds the durable: `/v1` opens once NATS is connected and the interest and session caches are warm (`envoy-listener /v1 open`, with `since_listening_ms`), and the role lane is a core-NATS queue subscription in the machine's group, `envoy-listener-<machine id>`, which NATS hands each role message to one member of, so the overlap forwards no message twice. The durable's bind is polled every 2 s (`subscribe failed, retrying`, logged at the first refusal and then once per 30 s), so it binds within 2 s of the old task's exit (`durable bound`, with `attempts` and `waited_ms`); a durable still held after 135 s ends the start with `subscribe failed after max attempts, shutting down`, as the backoff it replaced did. `/healthz` answers 200 `starting` until the bind, so `starting` no longer means `/v1` is closed. Every end of the bind wait other than the bind, that exhaustion, a refusal, a SIGTERM, is an ordered shutdown like any other, since `/v1` and the role lane already serve. Each request a starting gate refuses logs `request refused while starting` with `gate` (`webhook` or `v1`), `method` and `path`, the listener's only record of a caller it turned away. `packages/envoy/scripts/listener-deploy-probe.sh` watches a deploy from a client's seat (`packages/envoy/scripts/README.md`).
- If a session is not live in the registry, delivery fails and the message is NAK'd for retry (up to MaxDeliver attempts over the stream's MaxAge window).
- CI settlements are published by the listeners that receive GitHub webhooks, which in production is the Fargate listener behind the webhook load balancer (its deployment sets `ENVOY_WEBHOOKS=github,slack`). Only the GitHub route records checks, so a listener whose `ENVOY_WEBHOOKS` does not name `github` - the on-prem fleet's, which sets no webhook variable - opens no CI store: no `envoy_ci_state` bucket, watch or consumer, no summary loop and no `envoy_ci_legacy_records_held` gauge, and it logs `CI store not opened` at start. It must not scan that bucket. The watch delivers every record as it starts, and an on-prem listener reaches production NATS only through a relay: there the burst trips the server's 10 s write deadline (`Slow Consumer Detected`) and holds up the replies `store.Open` waits on past their 10 s deadline, so the start fails.
- A check_run or check_suite webhook writes the CI record of its commit in each pull request it names, so every check of a head writes one record, and the CI store combines concurrent observations of a record into one compare-and-swap write (`update`), and a burst of a pull request's checks costs far fewer writes than it has observations. A write retries a lost compare-and-swap, or a transient KV error, from a fresh read for up to two seconds (`recordBudget`). One that runs out answers every delivery in its batch 503, which Dispatch's redelivery sweep resends, and logs one JSON line at ERROR, `ci record exceeded its retry budget`, carrying `owner`, `repo`, `number`, `sha`, `checks` (the record's checks with the batch applied, or 0 when no attempt could read the record), `attempts`, `observations` (the deliveries it answered 503) and `error` (`cistore: record exceeded CAS budget` when the last attempt lost its compare-and-swap). It is an ERROR because each one means GitHub was answered 503, and it is meant for an alarm on the CI-record budget to count. The CI store's other lines go through the same JSON logger, so they carry the listener's `machine_id`.
- Every write of a CI record stamps it `schema: 1` (`encodeRecord`), so a record without the field was last written by a listener that settled only its pull request's head and left every other terminal commit unsettled. Such a record settles only in `[debounce, debounce + 5 min)` after its `last_event_at` (`handoverGrace`), which covers the time between the old listener's last tick and this one's first: a compose deploy of a listener that receives GitHub webhooks is stop-then-start, and its slow path to the first tick takes at least the startup floor in the table in `docs/solutions/architecture-patterns/envoy-ci-summary.md`, which lists the terms and their bounds. So a head that finished just before the old listener was replaced still settles, and the commits that listener declined to settle hours or days earlier never do. Every admitted record is at most debounce plus grace old (5 min 5 s at the 5 s debounce), by construction. Five minutes is the smallest round figure above that startup floor, and a wider grace would only make admitted settlements later: any width admits the non-head records the head-gated listener left in its last debounce plus grace, which this listener settles as it settles every commit, so the width bounds how late such a settlement arrives, not whether one does, while the aged backlog is held back by its age at any width shorter than its records' ages (5 minutes against the 7-day TTL, about 1 in 2,000). One predicate, `due`, decides it for the summary tick (before it reclaims a claim) and for `ClaimSettlement`, and the gauge `envoy_ci_legacy_records_held` on `/metrics` carries how many records the last tick held back, with one INFO line, `checks held back a head-gated listener's unsettled records, at least`, the first time a process holds any (a floor, since the loop does not wait for the CI cache to load). That is a decision, not an accident: an operator who finds a terminal record with `settled_emitted: false` and no `schema` is looking at that history, which the bucket's seven-day TTL expires. An observation that changes the record (a new check run, a re-run, a suite) stamps it and the commit settles as any other, and a stamped record of any schema is never held back, so a settlement pending across a restart of this listener still publishes. A record without a schema whose `last_event_at` is ahead of the listener's clock publishes once wall time reaches its band, and raising `ENVOY_CI_DEBOUNCE` moves the band's far edge, so a restart with a larger debounce admits the records in the added slice. A head-gated listener run after a rollback decodes a stamped record, ignoring the field, and its own writes drop it. Seven days after the last head-gated listener stops, no record without a schema remains.
- The `ENVOY_NOTIFICATIONS` duplicate window is 72 hours, matching the retained notification lifetime. Startup reconciles that setting with `UpdateStream`, so a Dispatch outbox retry after a post-publish crash cannot create another retained message while the original remains available.
- An envelope publishes under a JetStream MsgId of its dedupe key and topic when that key names its event: `contracts.DedupeKeyNamesTheUpstreamEvent`, which asks the envelope rather than its source name, and holds for a `github`, `slack` or `ghostwispr` key that is the source plus the envelope's own `SourceEventID` (the webhook normalizers' shape), for every `dispatch` envelope (LEGION-271), and for a key minted once for its message, from any source (`contracts.MintedDedupeKeyPattern`: the listener's own `publish.<id>` or `agent.<session>.<id>`, the same around the shared transport's UUID idempotency key, or either behind the role arbiter's forward mark), which only a re-send of that message repeats. It is `dedupeKeyNamesItsEvent` in `packages/contracts`, which every core-NATS host asks for its own dedupe. A webhook redelivery of an event the stream already holds (GitHub's and Ghost Wispr's resend under the original delivery id, Slack's retry under the original `event_id`) is dropped at publish and still answered 200, and each topic of one delivery's fan-out lands once. The case this covers is a first attempt that reached the stream but that the sender recorded as failed: a reply slower than GitHub's 10-second limit, or a 503 after part of a fan-out published. GitHub redelivers only the past three days, which lies inside the window. The rule reads the key because a source name proves nothing: the MCP bridge publishes under the source its configuration names, `github` included, with a key that is a hash of the resource URI and the summary, which two distinct events on one URI share whenever the read returns no text (LEGION-423); a CI settlement carries a key of the head and the record's generation, which a record recreated under that head can reuse with a different snapshot; and the Go daemon's outbox keys a notice by its row id, `legion-outbox:<row>`, which starts over with each new store (LEGION-426). None is a redelivery, and none is deduped. An envelope under any other key its caller chose carries no MsgId.
- A `bus.ConnectOwningStream` caller reconciles `ENVOY_NOTIFICATIONS`'s subjects at start by adding its own to the deployed list. Only the deployed services call it: the listener (including the on-prem fleet's) and Dispatch's server. A caller that only publishes or only tails - `natstail`, the MCP server, `envoy-dispatch`'s operator commands - uses `bus.Connect`, which neither creates the stream nor updates it. **Either connect refuses a NATS server that is not this machine's unless the run sets `ENVOY_ALLOW_REMOTE_NATS=1`**, decided from the URL before anything dials, because nothing distinguishes the deployed Dispatch from the same binary run out of a checkout: both read `natsUrls` from `~/.config/opencode/envoy.json`, which on an agent machine names production. Each deployment states its reach instead (`deploy/compose/*.compose.yml`, the production deployment's listener and Dispatch service definitions, the on-prem fleet's Pulumi), and each must carry it **before** an image whose binaries read it runs there, or that start refuses the shared NATS its deployment names and exits; setting it early is free, because a binary built before the variable ignores it (LEGION-249). Once every writer runs a build with this reconciliation, a restart during a rollout cannot drop a subject another deployment needs, except when two writers with different lists start within one read-update round trip (JetStream's stream update has no compare-and-swap). A start removes a deployed subject only when it overlaps a role lane (`notifications.role.>` or its exceptions twin) or one of the binary's own subjects (a widened, narrowed or split subject, which JetStream refuses beside it). In the second case the binary's shape wins, and a WARN names the dropped subject and every subject that replaced it. Each start also logs, at INFO, the deployed subjects it keeps without compiling them, which is the list the retire step works from. Retiring a subject is an operator step once no deployment compiled with it can start: `nats stream edit ENVOY_NOTIFICATIONS --subjects=... -f` (`docs/solutions/envoy/nats-jetstream-stream-ensure-only-adds-subjects.md`).
- Cross-machine route correctness depends on valid session registry entries with non-null ports.

## Listener API

| Endpoint | Method | Contract |
| --- | --- | --- |
| `/v1/messages/send` | POST | Sends to a live `target_session`. Dispatch uses `source: "dispatch"`, an idempotency key, and a `payload` JSON string whose frame is `{event, delivery}`; the response is the envelope plus `recipient` with the full target session ID, and `duplicate: true` when JetStream already held this message. A `source: "dispatch"` send publishes under a MsgId, and so does a send whose key was minted once for it (the listener's own, or around the shared transport's UUID idempotency key); only those can ever answer `duplicate`: a send that declares `github`, `slack` or `ghostwispr` carries a `SourceEventID` minted for it, which its dedupe key does not name (`contracts.DedupeKeyNamesTheUpstreamEvent`); the field is omitted (read as false) otherwise, which is also what an older listener answers. It means "the stream already held this MsgId", which includes the publish path's own reconnect retry whose first attempt landed and lost its acknowledgement. A message NATS cannot take whole is a 413 naming its size against the server's max payload, and a topic NATS does not accept (one holding whitespace or an empty token) a 400. The route takes the caller's `source` and idempotency key as given (`messageEnvelope`), so any caller can send a `source: "dispatch"` frame under the key a Dispatch send will use, which a core-NATS host then drops as a repeat when the real send or its Retry arrives. That key is not secret: a comment mention's is `<comment id>:<target>:<mode>` (`comment_delivery.go`), and the comment id rides the `comment.created` envelope on the issue's owner topic. It is accepted because every caller `apiAuth` admits (the shared bearer, or any service-account token the verifier accepts, whose claims it does not read) can already send any `source: "dispatch"` frame, with any content, to any live session; suppressing one Retry is a subset of that, and a Dispatch-only credential is the fix for the whole class. The listener is not the only route to that either: a NATS user that may publish on `notifications.agent.>` or `notifications.dispatch.>` reaches core-NATS hosts directly, and with no nkey configured (`internal/bus/nkey.go`) that is any process that can reach the server. |
| `/v1/messages/publish` | POST | Publishes a non-agent topic. An optional `dedupe_key` is used verbatim as the envelope's dedupe key (how a re-sent copy stays recognisable to the receiver's own dedupe); it is mutually exclusive with `idempotency_key` — both present is a 400 whose `expected` names both fields — it may not begin with the reserved forward mark `envoy.role.forward.` (a 400 naming `dedupe_key`; the role arbiter drops such an envelope on sight, so accepting it would be a 200 for a message that vanishes), it may not ride a `source: "dispatch"` envelope (a 400 naming `dedupe_key`: every host drops a repeat of a dispatch key, and Dispatch's outbox keys are sequential, so a chosen `dispatch-<next id>` would make hosts drop the real event; Dispatch's outbox publishes to the bus and its sends take the listener's key, so it never sets one here), and an empty string is absent; without either key the key is minted. A `notifications.role.<role>` topic requires a live holder and returns that session ID in `holder`. Its 404 adds `reason: "unclaimed"` for a role with no claim, or `reason: "holder_lapsed"` with the prior `holder`, `claim_released`, and a `last_seen` timestamp when the final heartbeat remains in its one-TTL diagnostic window. A message NATS cannot take whole is a 413 naming its size against the server's max payload, and a topic NATS does not accept (one holding whitespace or an empty token) a 400. |
| `/v1/roles/<role>` | GET | Returns the live role holder, including its capabilities and `last_seen`. A 404 has `reason: "unclaimed"` when no role claim exists; for an absent holder it has `reason: "holder_lapsed"` with the prior holder's ID, whether this lookup released the claim, and any final last-seen time retained for one TTL. |
| `/v1/roles/set` | POST | Claims a role for a live session and registers its role topic. Last-claim-wins by default. With `"soft": true` the claim lands only if the role is unheld, already this session's, held by a session that is no longer live, or held by the declared `previous_session_id` (the id a fork/branch continues); any other live holder answers `409 {error, role, holder}` and nothing changes. |
| `/v1/interests/subscribe` | POST | Persists session topics, route metadata, and optional delivery `capabilities`; registrations without capabilities persist `[]`. The response can include `warnings` when a GitHub repository has no retained events, and when a GitHub topic's token after its owner and name is not one of `contracts.GithubTopicKinds` or a wildcard. When a kind follows the extra tokens, or the name holds an empty token, the name was spelled with its dot, and the warning names the spelling Envoy publishes (`notifications.github.acme.site.io.pr.7.>` → `acme.site_io`). Otherwise the warning says the token is not a GitHub topic kind and names the kinds (`notifications.github.acme.widgets.checks.>`). |
| `/v1/interests/unsubscribe` | POST | Removes the supplied topics and returns them in `removed`. |
| `/v1/sessions` | GET | Lists live sessions; optional case-sensitive substring filters are `dir` and `title`. Rows include `roles`, `capabilities`, and `last_seen`. |
| `/v1/interests/` | GET | Lists persisted interests. |
| `/v1/interests/<session_id>` | GET, DELETE | Gets or removes a session's persisted interests. |
| `/v1/registry/<session_id>` | GET | Gets a live session registry entry. |
| `/v1/sessions/<session_id>` | DELETE | Removes a live session registration. |

`send` and `publish` accept optional `in_reply_to`, `supersedes`, `urgency`,
`expects_reply`, and `expires_at`, and `publish` additionally `dedupe_key`; empty optional
fields are omitted. `urgency` is `low`, `med`, `high`, or `blocking`; `expects_reply` is
`none`, `optional`, or `required`. A session id or role that becomes a KV key NATS would refuse
(`bus.EnsureKeyValue`) is a 413 when too long and a 400 when it holds an empty token or
whitespace, on every route that reads or writes one. Every `/v1` 4xx/5xx response, including
the startup gate's 503, is JSON: `{"error":"<message>","expected":["field"]}`. `expected`
appears when the caller must provide a field.

## Targeted Dispatch messages

Any authenticated caller — a browser session or a bearer naming its session in `actor` — can
create an issue message with `target: "session:<id>"` or `target: "role:<name>"` and
`delivery: "btw" | "aside" | "steer"`; a bearer-authored targeted message is authored by that
session, never by a human. A human may also create an issue-less, session-targeted message with
`POST /api/v1/agents/{session_id}/messages` `{body, delivery, in_reply_to?}`, and
`GET /api/v1/agents/{session_id}/messages` returns that session's issue-less and issue-anchored
targeted roots: the 50 whose thread moved last, plus every conversation holding a reply the caller
has not read, in one activity order (`greatest(root.created_at, newest reply)`), so the window
always covers what `unread_replies` counts. `unread_replies` and the window's unread term share one
definition, `unreadDirectRepliesCTE` (`api/unread_replies.go`), which both queries build on: the
count is its rows per session and the window is the 50 most active union its roots. Two predicates
that only agree by accident would let a reply outside the window be counted, never shown, and
then marked read by a watermark that only moves forward (LEGION-301). The unread term has no
ceiling, deliberately: a cap would reopen that defect at a higher threshold. It grows only while
the badge is ignored, since opening the view clears it - a practical bound, not a structural one,
and the same bound the root scan's cost has, both indexed by `messages_direct_roots` (`0051`).
Each root comes back with its deliveries, its reply chains (a reply in the chain carries its own
deliveries), and `unread`, that conversation's own verdict from the same fragment - the server's
answer to "does this hold a reply the caller has not read", so no client derives it from
timestamps. The scope is a root this viewer targeted at this session: a root the session only
received a delivery of (targeted at a role, or at another session) is listed by activity like any
other conversation and is never unread. The candidate roots are read as two indexed branches
unioned (`messages_session_roots`, `message_deliveries_session`), and their activity as one
recursive walk over every candidate's replies: an OR across `messages` and `message_deliveries`
can use no index, and a lateral walk per candidate estimates high enough to put the plan past
`jit_above_cost`, where each read pays for compiling the plan. Dispatch resolves a role holder
and checks the selected session's capabilities for every attempt, then makes the synchronous
listener send; `POST /api/v1/messages/{id}/deliveries` creates an explicit retry attempt (same
callers, same `actor` rule for bearers) in the `delivery` mode it names - the attempt's own mode
is the retry that cannot deliver the message twice, another mode is a genuine second delivery.
The targeted session alone uses
`POST /api/v1/messages/{id}/reply`, both for the attempt's automatic BTW response and for
`dispatch_message({ in_reply_to })` called with a bare message id and no `issue` - the only way
to answer a human's issue-less direct message, since every other message route hangs its message
off an issue. The tool sends `attempt: 1`, the attempt an issue-less message is created with,
and the executor decides the route from what the caller named rather than from the resolved
owner, so `LEGION_ISSUE` never files a direct-message answer on an unrelated issue
(`packages/envoy-client/src/dispatch-execute.ts`). A `dispatch://KEY/message/<id>` names the
issue its message lives on, so that form and any call naming an `issue` still post through
`POST /api/v1/issues/{key}/messages`. The tool always sends `?follow_up=true`, so once an attempt
is answered its next `dispatch_message({ in_reply_to })` with other text is the session's follow-up
(201, threaded under its first reply) and its result names that first reply in `details.follows`;
the result also says `dispatch_read({message})` reads the conversation back. Text the session
already posted in the conversation comes back marked `duplicate`, and the tool reports that nothing
new was posted. A Dispatch that predates follow-ups ignores the parameter (it would have refused a
body field, since the route decodes strictly) and answers a second reply with the stored one at
200 without posting, so the tool compares that reply's body with the one it sent and reports the
message as already answered rather than as a send. The host's automatic BTW answer
(`postDeliveryReply`) never sends the parameter.

`GET /api/v1/messages/{id}` reads the conversation any message belongs to, issue-less or not, by
the id of any message in it: `{message: <thread root with its deliveries>, replies: [...]}`, oldest
first. It is how `dispatch_read({ message })` reads a direct-message conversation back, since a
human's direct message belongs to no issue. A thread on an issue follows the issue's rule, exactly
as `GET /api/v1/issues/{key}/messages/{id}` does. An issue-less thread is a direct conversation: a
human reads any of them, and a bearer names its session in `?session=` (400 `SESSION_REQUIRED`
without it) and reads only one that session is in, whose root targets `session:<id>` or where it
authored a reply; any other is 403 `THREAD_FORBIDDEN`. The session is the caller's own claim, like
`actor` on a write, so this keeps a session from reading another session's direct conversation by
mistake and is not an authorization boundary. Direct-conversation text also reaches every authenticated caller through `GET /api/v1/events`; nothing in Dispatch restricts it by session. The tool sends the host's session id.

Messages thread: `in_reply_to` names a message in the same conversation - a message of the same
issue, or, for `POST /api/v1/agents/{session_id}/messages`, an issue-less message whose thread
root targets that same session (400 `MESSAGE_INPUT` otherwise) - and the reply's event and
delivery frame carry `reply_body`, the parent's first 160 characters. A reply is recorded as
`message.answered`; the broker's `Notify` treats it exactly like `message.created` (quiet when
the payload has a `target`, else the user-actor rule), so a human's reply on a plain agent message
wakes the issue's route and the parent message's author (`notifications.agent.<session>`,
correlated `re:` the parent with `reply_body`) instead of being recorded silently. A human's reply that names no `target` inherits the thread's:
when the root of the reply's ancestry was targeted, the reply is delivered to that target in the
mode of the thread's most recent delivery attempt, exactly like a fresh targeted message (its own
`message_deliveries` row and `message.delivery` event), so a human's follow-up on an agent's
answer reaches that agent and the agent answers it through `POST /api/v1/messages/{reply id}/reply`.
A session's own reply never inherits (the target would be itself), and an explicit `target` wins.
Every reply event carries `thread_target`, the target of its thread root, because that root is
the conversation `GET /api/v1/agents/{session_id}/messages` groups the reply under: a session's
own reply has no target of its own, and a consumer keyed on the event's `target` alone would
never see it.
`POST /api/v1/messages/{id}/reply` with a `body` is accepted on a `failed` attempt as well as a
`sent` one — the session answering is proof the message reached it, whatever the receipt said
(a stale plugin, `nats: invalid jetstream publish response`, an error the session itself reported
earlier) — and records the attempt as `sent` with no error and the reply's id; an `error` on an
already-failed attempt returns the stored attempt unchanged. On an answered attempt an `error`, or a
`body` sent without `?follow_up=true`, is a retry: it returns the stored reply (200) without
posting, so a frame handed to the session twice never posts a second automatic answer. With
`?follow_up=true` a `body` equal to the stored reply's or to an earlier follow-up returns that
message (200) without posting, and any other `body` is the session's follow-up, stored under its
first reply with its own `message.answered` event (201). Every answer that posted nothing carries
`duplicate: true` beside the message (`replyRead`). The attempt keeps naming the first reply.
`follow_up` is `true`, `false` or absent; any other value is `400 MESSAGE_INPUT`, so a caller's
typo is refused rather than answered as a retry.

Every attempt records `requested_by`, the actor whose send opened it (the message's author, the
person or bearer who retried it, the human whose reply inherited the thread's target; kept on a
resume; null on a row from before migration 0054), and at most one attempt of a message records
`accepted_at` and `accepted_as: "user_turn"`: the session it went to took it as its user's own turn,
through `POST /api/v1/messages/{id}/deliveries/{attempt}/accept` (its conditions are that route's
row in `packages/envoy/cmd/dispatch/AGENTS.md`, whose requester checks read `requested_by`). What
a session does with the answer is `packages/pi-envoy/AGENTS.md`'s.

A human reaches many sessions at once with `POST /api/v1/broadcasts`
`{body, delivery, session_ids, idempotency_key}`. A broadcast is a grouping over the targeted
messages above, not a second delivery mechanism: one row in `broadcasts` plus one issue-less
message per recipient carrying its `broadcast_id`, all in one transaction, then the ordinary
`deliverMessage` path per recipient - so each recipient's attempts, retries and replies are
exactly a single targeted message's. Recipients are judged against one listener read: a
selected session that is not live, or does not advertise the chosen mode, is excluded before
anything is written and named in the response's `excluded`, never switched to another mode
(the mode is part of what the sender said). Exclusions are not stored; a send with no
reachable recipient is `400 BROADCAST_EMPTY` and writes nothing. The send answers 201 as soon
as the messages are committed and delivers behind the request, four recipients at a time, each
worker on its own `store.WithTransactionTracking` context derived from the server's lifetime
(the pool's one-connection guard is per context, and a request's context would strand every
recipient after the one in flight when a tab closes or a deploy shuts the server down). Each
recipient starts with attempt 1, opened pending and unclaimed in the same transaction as its
message, which a delivery worker then claims and settles.
`GET /api/v1/broadcasts` lists the newest sends with recipient and reply counts, and
`GET /api/v1/broadcasts/{id}` reads every recipient's message, attempts and replies in the
order the send named them; all three routes are human-only, like the one-session route they are
built from.

Every send carries an `idempotency_key` naming that one send (LEGION-446): letters, digits,
`.`, `_`, `:` and `-`, at most 128 characters; a missing or malformed key is
`400 BROADCAST_INPUT`, a longer one `400 CAP_EXCEEDED`. A key belongs to the human who sent it:
its `broadcast_idempotency_keys` row (migration `0055`) is keyed on `(login, idempotency_key)`,
`login` being `canonicalLogin`, so one person's key is never another's and `Alice` is `alice`.
The row also stores a SHA-256 of what the send asked for - its body, mode and requested
sessions in order, after validation trims and de-duplicates them. A repeat (the same human, key
and request) is answered `200` with the broadcast the key made, in the create response's shape
and read as `GET /api/v1/broadcasts/{id}` reads it, its `excluded` recomputed as the requested
sessions it has no recipient for, each with the reason `excluded by the original send`, since
the original reasons are not stored. A reuse for a different request is
`409 BROADCAST_KEY_REUSED`, whose text says the request was not sent and whose body names the
broadcast that used the key as `broadcast_id`; nothing is written. The key is looked up before
the listener's registry is read, so a retry of a send that landed is answered even while the
listener is down. Two requests carrying one key at once both miss that lookup and reach the
write together: the key row is inserted in the broadcast's own transaction, so the second waits
on the first's uncommitted row, is refused it (`23505`) once the first commits, rolls back
everything it wrote and answers the first's broadcast - one broadcast, one frame per recipient.
A replay reads the broadcast as it stands: a recipient whose attempt a process death stranded
pending before delivery comes back pending, for the broadcast view's same-mode retry to resume,
so a `200` says the broadcast exists, not that delivery is under way. A key is recognised for as
long as its broadcast exists; there is no window, and a broadcast from before `0055` has no key.

The issue stream retains the targeted `message.created`, `message.delivery`, and
`message.answered` events for the Conversation card. Issue-less targeted-message events have no
issue owner, use sequence `0`, and reach their recipient through the synchronous listener send;
the outbox marks them published without republishing an agent-topic envelope, while the browser
receives them through the server's SSE stream. They are excluded from global `/search` and issue
reference closures. Targeted issue `message.created` skips bound-route and author fan-out because
the synchronous listener call records the sent or failed attempt instead of blind republishing.

`/v1` accepts two bearer kinds: the `ENVOY_API_TOKEN` shared token, and — when `ENVOY_OIDC_ISSUER` and `ENVOY_OIDC_AUDIENCE` are both set, one without the other being a startup refusal naming the missing variable — a projected service-account token that issuer signed for that audience. The exempt paths (`/healthz`, `/metrics`, `/webhook/*`) require neither. `/v1` is open only when neither is configured, so an empty `ENVOY_API_TOKEN` opens it only while no verifier is configured, and a non-loopback listener refuses to start with neither unless `ENVOY_API_ALLOW_UNAUTHENTICATED=1` is the temporary Fargate transition flag. The boot log names what is on: `shared token`, `oidc <issuer>`, both, or `disabled`. A service-account token the verifier refuses is one `listener: service-account token rejected` warning carrying `oidc.Reason`'s class and the path — never the bearer, and never the class in the 401 body, which would tell an unauthenticated caller which credential the listener is configured for. The classes are `malformed`, `issuer`, `audience`, `expired`, `cancelled` (the request ended before the verdict — a disconnect or a deadline; the class is decided on the context, so a bad token presented on a connection that is already gone lands here too, and `cancelled` means "no verdict is trustworthy", not "the token was sound"), and `signature`, which is the residue: an issuer whose key set cannot be retrieved still lands there, because go-oidc flattens that error past `errors.Is`.

## Topic shapes

- GitHub repository topics start with `notifications.github.<owner>.<repo>`, each name one
  segment with a dot written `_` (`sjawhar/.github` is `notifications.github.sjawhar._github`). A
  ref or workflow file name in a topic is one segment the same way, with whitespace, `*` and `>`,
  which a published subject may not hold, written `_` too (`SanitizeSubjectSegment`).
  Pull requests use `pr.<n>` for lifecycle (a closed event carries
  `merged`, `merge_commit_sha`, `merged_by`, and `head_sha`), plus
  `pr.<n>.comment`, `pr.<n>.review`, `pr.<n>.mention`, and
  `pr.<n>.checks` when a commit's checks settle: every commit of the pull request, its current head or not, whose `sha` the settlement carries (a head pushed with GitHub's `skip-checks` trailer runs no CI, so the commit it replaced settles for it, and consumers decide which commit a settlement stands for). Check settlement is at-least-once: a settlement can be followed by a `superseded_settlement: "true"` payload. Every settlement carries its attempt set `check_runs` — the latest GitHub check-run id per check name, sorted by name — plus the listener's `generation` (the record's state version, which rises with every write that changes the record's snapshot, once per write however many observations that write folds in) and `snapshot` (the record's hash). Consumers order settlements of one commit by the attempt set, compared per shared name: no id lower and some id higher (or a new name — a new name counts as higher) is newer; every shared id equal and no new name is the same set; no id higher, no new name, and some id lower is older; anything else (a higher or new alongside a lower) is a mixed view and is dropped as a conflict (names only in the stored set are ignored — a check can vanish from GitHub's view, and a record recreated after the seven-day KV TTL starts sparse). Within one producer record per-name ids never decrease, and a consumer's fence is the per-name maximum over every view it has accepted — an accepted set merges into the fence, nothing is pruned — so the fence never decreases either: a newer attempt is newer whatever its completion time, no timestamps take part in ordering, and a name an incomplete view omitted cannot later reappear as new. At the same set the listener's `generation` orders its own settlements: lower is stale; equal is a duplicate when the `snapshot` matches and otherwise a conflict (an equal pair with a different snapshot cannot occur within one record's lifetime; a recreated record may reuse one and is dropped). A live settlement is a possibly incomplete view of the head (a missed webhook, a record recreated after the KV TTL): it decides the outcome of every name it reports — at any id the ordering accepted, including the same run observed in place — and says nothing about the rest: a known failure among them stands (the consumer keeps failure names, not a per-name status map), and the head is red while any failure remains. A consumer that reconciles a verdict from GitHub's rollup compares the rollup's attempt set the same way, but GitHub's read is complete: its failing check runs and failing commit statuses replace the stored ones wholesale. Statuses have no check run and the listener never sees them, so a consumer keeps them apart from check-run failures: a check run that shares a status's name cannot retire it — only GitHub does (likewise a deleted check's failure). A newer rollup set merges into the fence and takes the identity (no listener generation); the same set applies GitHub's verdict and keeps the listener identity for duplicate detection; an older, mixed, or empty-over-fenced set is ignored. A terminal read (green or red) then holds the tie at that set: a live settlement at the same set is accepted only if its effective outcome — the check-run failures it reports plus the stored ones it omits and the stored commit-status failures — agrees with the reconciled verdict, refreshing the listener identity without releasing GitHub's authority; a disagreeing one is stale whatever its generation until the set advances; a pending or cancelled-only read uncertifies a green head, leaves a red one untouched, and holds nothing — it releases any authority held at that set — so the terminal live settlement that follows applies at once, subject to the ordinary generation and duplicate rules (a replay or a lower generation still does not apply). Pending is therefore not a commutative join: a pending read after a live green uncertifies it until the next terminal view. Two remainders. An in-place conclusion change on an existing run id: GitHub's view stands and the listener's is recovered by the next successful, non-skipped read at that set — the dropped delivery is not replayed. A check whose highest run is deleted on GitHub: the fence keeps that id, so a rollup reporting a lower run under the same name is older until a newer run appears. A consumer that orders head changes by the PR's `updated_at` (GitHub's second resolution) accepts a read of a different head at an equal clock — a stale read returning the previous head within the same second as its replacement rewinds that consumer until its next accurate, non-skipped read. A head publishes only when at least one check has a positive run id; legacy checks without one remain in the status groups and failing names but not in `check_runs`. A legacy in-progress check whose completion is never observed holds the head unsettled until it reruns; rerun the affected check to release it. A head whose CI record overflowed its bounds (`maxRecordBytes`, `maxSettlementBytes`), or whose settlement NATS refused, never settles from the listener again; one that had settled before keeps that last settlement. `workflow.<file>.<action>` carries only runs without an associated
  pull request.
- A branch or tag push publishes `push.branch.<ref>` / `push.tag.<ref>` with the payload fields
  `kind: "push"`, `repo`, `ref`, `after`, `before`, `pusher`, `head_subject` (the head commit's
  first non-empty message line, capped at 2,048 runes with a trailing `…` so the envelope stays
  under NATS's max payload), `commit_count`, `compare_url`, `changed_paths` (the unique paths
  across every pushed commit's `added`, `removed`, and `modified` lists, in first-seen order,
  newline-separated, at most 100 of them and 32,768 runes of text, each path listed whole or not
  at all; omitted when no commit is listed, since the payload drops empty strings), and
  `changed_paths_truncated` (`"true"` when the list stopped at either cap with a unique path left,
  else `"false"` — present on every push, so
  its absence alone tells a consumer the listener predates the field), and `forced` (`"true"` or
  `"false"`, GitHub's own flag, present on every push the same way). A forced push's commits are
  listed from the merge base, so its `changed_paths` do not describe what it did to the head it
  replaced. Envoy forwards what GitHub sent; what counts as a handoff-only push is the Legion
  daemon's rule, not the listener's.
- Every other text a webhook payload copies from its body (a title, a ref, a path, a workflow name,
  a URL) is capped at 2,048 runes with a trailing `…` too (`maxEnvelopeTextRunes`; a comment's or
  review's body is cut there without one and says so in `body_truncated`), and so is every text a
  check run copies into the CI store (its name, URL, status, conclusion and times), so no envelope
  or record grows with the body it came from. The record is keyed by check name, so a name past the
  cap keeps a digest of the whole name after its `…`, and two names that share their first 2,048
  runes stay two checks. What NATS still refuses however often it is sent is refused
  (`bus.ErrRefused`): an envelope past the server's max payload or a subject past the server's 4 KiB
  protocol line, over which the server would close the listener's connection (`bus.ErrTooLarge`,
  naming the size), or a subject holding whitespace or an empty token, which no stream matches
  (`bus.ErrInvalidSubject`). Every KV bucket is opened through `bus.EnsureKeyValue` or
  `bus.OpenKeyValue`, whose handle checks its keys the same way, since a KV call builds its subjects
  from the key: a key that would take one past the protocol line, or holding an empty token, is
  refused before anything is sent. A write is held to the longest subject its key makes (a watcher's
  create request), so a key written now stays readable, watchable and deletable; any other call only
  to its own subject, so a key an earlier build stored past that bound still lists and deletes. The
  stores skip such a key where they cannot read or rewrite it, with a WARN, rather than fail a
  start, a sweep or a caller's own request, and the reapers delete it. The webhook is answered 422,
  which Dispatch's redelivery sweep takes as terminal, and logged `<source> publish refused` (or
  `github ci record refused`); any other failure stays a 503 logged `<source> publish failed`. A
  commit's CI record is bounded at 384 KiB (`maxRecordBytes`, about 1,300 checks) and its settlement
  at 960 KiB (`maxSettlementBytes`; a failing check's `"` costs three times as much there), since
  GitHub allows 50,000 check runs in a suite: a check past either is refused the same way, and the
  record is marked `overflowed` and never settles again. A commit that had already settled keeps its
  last settlement, which nothing supersedes, so a consumer holding it learns about the refused
  checks only from GitHub's own read. A settlement NATS refuses anyway (a server whose max payload
  is set lower) marks the record overflowed too, logged once as `checks settlement refused`, rather
  than being published again every tick.
- A `pull_request_review` payload carries the review's own `commit_id` and the PR's current
  `head_sha` so consumers can tell whether the review is at head, and `submitted_at` (GitHub's
  RFC 3339 time) and `review_id` (GitHub's review id as a decimal string), so consumers can order
  reviews by when they were submitted, then by id, rather than by delivery. The id alone is not
  enough: GitHub assigns it when a review is created, and a pending review keeps it when it is
  submitted later. `pull_request_review_comment` carries `head_sha` too, and all three
  comment/review events set `legion_footer: "true"` when the uncapped body contains a
  `<!-- legion:` worker footer.
- NATS `>` matches one or more trailing tokens, not its base subject. A subscription to a concrete
  `<subject>.>` is registered as the pair `<subject>` and `<subject>.>`, so the recommended
  per-PR default receives lifecycle plus child events.
- Dispatch issue events use `notifications.dispatch.issue.<KEY>.<type>`, carry every event, and
  are retained in JetStream; `notify` only controls agent wake and routing the same envelope to
  `notifications.role.<role>` or `notifications.agent.<session_id>` (the issue's route, which also
  applies to a comment or ask anchored on the issue's own artifact). An ask-scoped event -
  `ask.answered`, `ask.edited`, `ask.handed_back`, `ask.resolved`, and `comment.created`,
  `comment.resolved`, `comment.reopened` or `comment.edited` carrying an `ask_id` - is
  additionally published straight to each follower's `notifications.agent.<session_id>` regardless
  of `notify` and of the issue's route, so a session-authored reply on an ask reaches its followers
  even though it wakes nobody through the issue topic; the acting session is skipped and a session
  already reached by the issue's route is not sent the envelope twice. A quiet `ask.edited` (a
  version move of an approval request already waiting on its agent, `quiet: true`) has `notify`
  false and reaches no follower, as a human's unnamed `artifact.version` reaches nobody. A
  notifying comment-thread or message-reply event without an `ask_id` keeps the author routes:
  a comment reply's
  thread-root author and its direct parent author when different; a message reply's direct parent
  author; and, for a bare resolve or reopen, the resolved comment's own root author. An author
  route is skipped for a human author and for a session replying to its own thread.
  `ask.follower_added` and `ask.follower_removed` also route directly to the session they name,
  since a newly added follower is not subscribed to the issue topic by default.
- Dispatch document events use `notifications.dispatch.document.<PROJECT>.<slug>.<type>` and
  are retained in JetStream. Consumers subscribe or tail that document topic directly; a document
  has no delivery route, so the same author routes above are the only way an involved session
  learns about a human reply, resolution, reopening, or edit on its ask or comment thread.
- Dispatch project events use `notifications.dispatch.project.<PROJECT>.<type>` and are retained in JetStream. They have no issue route; project, repository-setting, and user-interface state consumers refresh from the event stream.
- Direct agent topics use `notifications.agent.<session_id>`.
- Role topics use `notifications.role.<role>` and are normally published to,
  rather than subscribed to by role holders.
- A failed control delivery publishes
  `notifications.envoy.exceptions.<original-topic>`.

## Secrets broker

AGENTC-393's secrets broker (`cmd/broker`, `internal/broker/`) issues short-lived secret grants
and key-bound launcher credentials to enrolled agent sessions and pods; `cmd/agent-secrets` is its
client (a box's or pod's own key, or a host session's `cmd/agent-secrets-helper`), which enrolls a
runtime, requests grants, polls a pending decision to completion, and either prints session/grant
state (`self`, `status --json`) or `syscall.Exec`s a command with the granted values injected into
its environment. The broker holds no Dispatch credential and opens no Dispatch ask anywhere. Every
human decision — approving or denying a secret request, approving or denying a machine login,
revoking a grant — reaches the broker's UI routes from Dispatch's server, carrying the UI bearer
and the deciding person's Dispatch login, their email, in the body's `approver` field. The bearer
vouches for that login: Dispatch fills it from its own signed-in session, never from the browser,
and the broker checks it against the record's approver and records it on the decision event. The
UI bearer is therefore an approval credential, and keeping it and Dispatch's identity closed to
agents is the deployment's job. `internal/broker/enroll` turns a launcher credential into a leased
enrollment keyed by the caller's own signing key thumbprint (and, for a pod, a projected
service-account token). An operator credential's enrollment is its operator's, the email of the
person who approved its machine login: a launcher may leave `operator` out of the enrollment, and
one naming anyone else is `403 OPERATOR_MISMATCH`. A live enrollment is unique per launcher
credential, runtime id and slot: `POST /v1/enrollments` takes an optional pod-only `slot`
(`^[a-z][a-z0-9-]{0,62}$`, else `400 INVALID_SLOT`, and a slot on a box or host is refused the
same way) naming one of several independent identities in one pod. The launcher whose proof
authenticates the enrollment chooses the slot; a session's proof cannot enroll anything
(`401 LAUNCHER_INVALID`). So each slot of a pod holds its own key, lease, requests and grants,
while a pod's `runtime_id` stays the pod UID its token proves. Omitted or `""` is the runtime's
one enrollment, every box's and host's. The same key
in the same slot gets its live enrollment back (200), a different key in a live slot is `409
ALREADY_ENROLLED`, and the rules never see the slot: every slot of a pod matches on its verified
service account alone. Migration 0007 is forward-only: an older broker binary's conflict lookup
reads one live row per runtime id, unsafe once a pod holds two slots, so the binary is never rolled
back past it once a slotted enrollment exists. `internal/broker/rules` evaluates
`agent-secret-rules.yaml` policy per request (the AGENTC-393 overview document is its contract; a
file that still has an `approvers:` section is refused, naming the removal); `internal/broker/proof`
authenticates a session's or a launcher's signed request against its live enrollment or
credential; `internal/broker/machine` decides typed-code machine logins and mints the launcher
credentials they approve; and `internal/broker/secrets` reads the granted value from AWS Secrets
Manager, or a fake local file for development.

The client finds its session in `AGENT_SECRETS_KEY_DIR` (a box's or pod's `key.pem` and
`enrollment`) or `AGENT_SECRETS_HELPER_SOCK` (a host session's helper), beside `AGENT_SECRETS_URL`.
Unset, each falls back to its launcher's path, `$XDG_RUNTIME_DIR/agent-secrets` and the
`helper.sock` inside it, used only when that file is there; a pod sets its variables and has no
`XDG_RUNTIME_DIR`. The exec form's command keeps exactly those three variables, so an
`agent-secrets` call it makes is the same session's. A box's key and enrollment arrive after the
box starts, so its launcher writes `enrollment.pending` into the key dir before the box starts and
removes it once it has written `enrollment` or `enrollment.error`. While that marker is there and
younger than 160 s by mtime, a call waits for `key.pem` and `enrollment`, up to
`AGENT_SECRETS_ENROLL_WAIT` (default 20s), then fails with its ordinary error; with no fresh marker
nothing waits, and the Go shim never writes one, so a pod never waits. `agent-secrets identity`
answers locally, with no broker call and no registration, whether the calling process has a
session identity (exit 0 for a `key.pem`, a fresh marker, or the helper's sign probe answering OK
or NOT_ENROLLED; exit 1 otherwise, with a notice when a helper is expected but cannot be asked),
for callers that choose between the broker and another backend. A helper holding no launcher
credential, from every restart until the operator logs the machine in, enrolls no one: its sign
and sign-request answer NO_CREDENTIAL, so `identity` exits 1 and every other command fails, each
with the same not-logged-in notice naming `agent-secrets launcher login`. The login that installs
a credential wakes every registered session's enrollment retry, so those sessions reach the broker
within about a second of it rather than when a backoff of up to a minute comes round. A session
whose renew the broker refuses (its lease lapsed) stops counting as enrolled at once, and its
enrollment becomes the session's lapsed id; so does a re-pinned session's recorded enrollment
after a helper restart. While it is lapsed and the helper holds a credential, sign answers
NOT_ENROLLED naming the refused renew, and `register --wait N` waits for its re-enrollment. The
helper revokes a lapsed id before the session enrolls again, and while the session lives its
record keeps that id until the revoke lands, so a restart meanwhile, even a second one before any
login, still revokes it. A session that ends first takes its record with it and hands the id to a
bounded revoke (three tries); before a login those fail, and the broker's sweeper ends the id once
its lease lapses. A revoke refused 403 `OPERATOR_MISMATCH` (an enrollment made under another
operator's launcher credential) counts as done, and the session enrolls afresh.
`agent-secrets launcher login-status`, which the helper answers, exits 0 while the helper holds a
launcher credential and prints `issued`. A
re-login that is denied, expires unapproved or is still pending leaves the credential an earlier
login installed in place, and the helper keeps enrolling sessions with it, so login-status still
exits 0 and prints `issued`, and stderr names the most recent login and its code
(`credential_held` beside `login_state`, the most recent login's state, which `launcher login`'s
own poll reads). A helper from before that field reports only the most recent login, so there a
denied re-login still reads `denied` and exits 1 until the helper restarts on a release that
carries it. While a credential is held, login-status's last stderr line says when it expires and
how long that is from now (`credential_expires_at`, from the `expires_at` the broker's issued poll
carries); the broker has no renewal, so a new machine login a human approves must replace it before
then, and a helper or broker from before that field gets a line saying the expiry is unknown.
With no credential held, login-status exits 1 and prints the most recent login's state
(`pending`, `denied`, `expired`, or `none` before any login), except once the helper drops the
held credential, because the broker refuses it (401 `LAUNCHER_INVALID`, which it answers for an
expired or revoked credential and for any launcher proof it cannot verify, such as clock skew or
an `AGENT_SECRETS_URL` that is not the broker's public URL) or because it reaches the expiry the
broker named: from then until a login starts or settles, the helper reports every state but
`pending` as `expired` (`login_state`), the word the dotfiles launcher gate matches, so an older
client exits 1 on it too, with `login_refused` and the cause (`credential_dropped`). Stderr says
why the helper holds no credential in the words its journal uses for the same state
(`helper.NoCredentialReason`): a login awaiting approval, the cause it dropped the credential for,
a broker refusal on a helper from before `credential_dropped` (which sets `login_refused` only for
one), a denied login, a login that expired before anyone approved it, or no login since the
helper started. A re-login pending at the drop reports its own outcome once it settles, so its
`launcher login` prints `denied` for a denial. The helper logs every change of the credential:
`machine login issued` (credential id, its `expires_at`, and the operator the login was signed
with) when a login installs one, a WARN that it expires soon a day before its expiry (at once when
less is left), and one ERROR when it drops it, `<cause>; cleared: no session can enroll until a
human approves a new machine login`, the cause being `the launcher credential reached its expiry`
(with the expiry) or `the broker refused the launcher credential (…)` (with the broker's code); a
broker that names no expiry gets a WARN that the helper cannot warn ahead. A refusal of a
credential a newer login has already replaced drops nothing, and the enrollment it failed retries
at once with the new one after the ordinary `enroll failed; retrying` WARN. While it holds none,
every enrollment attempt, a session's or an `enroll-box`'s (and every revoke a re-pinned or lapsed
session needs before it enrolls), logs at ERROR `session cannot enroll` with that `why`; an
attempt that failed for want of a credential a login has installed since logs the WARN instead.
SIGINT or SIGTERM, from systemd or anything else, stops the helper with exit 0 after a WARN
`agent-secrets-helper stopping on a signal` naming the signal, how many sessions it had
registered (`sessions`) and whether it held a launcher credential (`launcher_credential`); the
dotfiles unit restarts it unless systemd itself stopped it (`Restart=always`). The helper writes
these lines to stderr as slog text, so journald stores the ERROR lines at priority 6 (info), and
`journalctl -p err` does not list them.
`agent-secrets --version` and `agent-secrets-helper --version` print the release tag the release
job stamps in (`internal/buildversion`), `devel` for any other build, and the helper's startup
line (`agent-secrets-helper listening`) carries the same version.
`register --wait N` answers at once while the helper holds no launcher credential, so the dotfiles
launcher gate (`scripts/agent-secrets-session`) can pass `--wait 10` without first checking that
login-status says `issued`, once the pinned release carries that answer and the helper has
restarted on it; its login-status probe stays, since it also finds a helper that does not answer.
Against an older helper an unconditional `--wait 10` stalls every launch 10 s while no credential
exists. With `--wait N --exec`, a session the helper cannot enroll for want of a credential starts
with a warning that its `agent-secrets` calls fail until the machine is logged in.

`config.Load` (`internal/broker/config/config.go`) reads the broker's `BROKER_*` environment:
`BROKER_LISTEN_ADDR` (default `127.0.0.1:13380`), `BROKER_DATABASE_URL` (required; a literal
`${BROKER_DATABASE_PASSWORD}` placeholder is substituted, URL-escaped, from
`BROKER_DATABASE_PASSWORD` — naming the placeholder without the variable, or the variable without
the placeholder, is refused naming both), `BROKER_PUBLIC_URL` (required; absolute URL, no path —
the broker's own address, the request object's `aud` and the launcher proof's `htu`),
`BROKER_UI_TOKEN[_FILE]` (required — the 32-byte bearer shared with exactly Dispatch's server; it
proves the caller is Dispatch, and Dispatch vouches for the approving login each decision names),
`BROKER_RULES_FILE` / `BROKER_RULES_S3_URI` (exactly one; the latter `s3://<bucket>/<key>`),
`BROKER_K8S_OIDC_ISSUER` / `BROKER_K8S_OIDC_AUDIENCE` (set together or not at all),
`BROKER_ENVOY_URL` (optional; turns on best-effort wake notifications to the requesting session
through Envoy's `/v1/messages/send`, sent with `BROKER_ENVOY_TOKEN` — read only when the URL is
set, and not itself required at startup), `BROKER_LEASE_SECONDS` (default 900, max 3600),
`BROKER_PROOF_SKEW_SECONDS` (default 60, max 300), `BROKER_MAX_GRANT_SECONDS` (default 43200, max
43200), `BROKER_RULES_RELOAD_SECONDS` (default 300, max 3600), `BROKER_LAUNCHER_CREDENTIAL_SECONDS`
(default 604800, max 2592000 — a minted launcher credential's own lifetime; past it the holder
re-runs login, new key, new code, new human approval), `BROKER_SWEEP_SECONDS` (default 5, max 60 —
`requests.Sweeper`'s tick interval, the poller's replacement), and `BROKER_TRUSTED_PROXY_HEADER`
(optional; names a request header, e.g. `X-Forwarded-For`, the launcher-credential rate limiter's
per-address bucket trusts for the caller's real address — its last comma-separated entry, the hop
your own reverse proxy appended, never an earlier client-supplied one. Unset, the default, keys on
`r.RemoteAddr` directly, correct only when the broker is reached without a proxy in front of it;
behind one — this broker's documented production shape, the shared internal ALB — `r.RemoteAddr`
is the proxy's own address for every caller, collapsing the per-address bucket into one shared by
everyone unless this variable is set). `BROKER_UI_TOKEN` and `BROKER_ENVOY_TOKEN` follow the
broker's `_FILE` secret-loading convention: `<NAME>_FILE`, when set, names a file whose trimmed
contents win over a bare `<NAME>` — with both set, the file wins silently, nothing is refused — and
a named-but-unreadable or empty file is a startup error naming the file, never a silent fallback to
an unset value. `config.Load` refuses to start naming a stale removal still set in the
environment — the removed `BROKER_DISPATCH_URL`, `BROKER_DISPATCH_TOKEN[_FILE]`,
`BROKER_DISPATCH_PROJECT` and `BROKER_ASK_POLL_SECONDS` (the broker holds no Dispatch credential
and asks/issues nothing), and `BROKER_UI_ORIGIN` (approval is by Dispatch login, so the broker
checks no WebAuthn origin) — so a stale deployment fails loudly rather than silently running on
configuration that means nothing any more. It also refuses: a missing required variable; a
`BROKER_PUBLIC_URL` that isn't an absolute URL with no path; both or neither of
`BROKER_RULES_FILE`/`BROKER_RULES_S3_URI` set; a `BROKER_RULES_S3_URI` without an `s3://` prefix;
exactly one of `BROKER_K8S_OIDC_ISSUER`/`BROKER_K8S_OIDC_AUDIENCE` set; and a `_SECONDS` variable
that isn't a whole number between the min and max its row of Load's `ints` table gives
(non-numeric fails the same check as out of range). `cmd/broker/main.go` reads one thing
`config.Load` does not: when `BROKER_RULES_FILE`
selects local rules (rather than `BROKER_RULES_S3_URI`, which pairs with AWS Secrets Manager for
secret values), it requires `BROKER_FAKE_SECRETS_FILE` and refuses to start without it — a
local-dev-only path. The broker takes no flags, and refuses any flag it is given.

The docs site's broker reference pages are generated at site build from this source by
`cmd/broker-refgen` (through `docs/site/generators/broker-reference.sh`), which fails the build on
an undocumented item: every `routes()` row needs a comment above it saying what the route does,
every adapter a reader label in refgen's `credentialLabels`, every handler's success answer a named
response struct of `internal/broker/api` passed to `writeJSON` (never a map literal; the one success
without a body is `w.WriteHeader(http.StatusNoContent)`, and any other `WriteHeader` status is
refused), every JSON field of a request or response body (and of an object it holds) a doc comment
saying what it is, a response type that is one of several answers of a route a doc comment saying
when (a branch on a boolean the route passes as `true` or `false` counts only where it is taken,
so approve and deny each document their own answer), every `Config` field a doc comment
opening with the `BROKER_*` variables it reads and a colon (`BROKER_FAKE_SECRETS_FILE`'s is in
`cmd/broker/main.go`; any `BROKER_*` name `config` or `cmd/broker` spells out as a string counts as
read), every `removedVars` row a reason, and every helper `Code*` constant and `agent-secrets`
`exit*` constant a comment. It also checks where those codes are produced: every helper `Response`
literal that is not
`OK: true` must name its fields and set `Code` to a documented `Code*` constant, and every
`int`-returning function in `cmd/agent-secrets` (and every `os.Exit` there) may return only `0`, `1`,
a documented `exit*` constant or another such function's result. The CLI reference is the built
binaries' own `--help`, so every form must answer `-h` with exit 0.

`internal/broker/api/routes_table.go`'s `routes()` is the one list of the broker's 19 HTTP routes —
a new route is a new row there, never a bare `mux.HandleFunc` — and its own comment says the
contract for every row is the AGENTC-393 overview document. Each row's handler is
wrapped by the adapter for its authentication (`public`, `launcherAuth`, `sessionAuth`, `uiAuth`),
which fixes both the credential `server.authenticate` checks and the caller the handler receives (a
launcher `enroll.Credential`, an enrollment id, or nothing at all for a UI route — the UI bearer
proves the caller is Dispatch, so every UI handler takes its subject from the path or body: a
decision or a revoke names the deciding human in `approver`, which Dispatch filled from its own
session, and an empty one is `400 APPROVER_REQUIRED`), so a handler cannot be wired to the wrong
kind of caller. A bad
credential is a 401 (`LAUNCHER_INVALID`, `PROOF_INVALID`, or `UI_INVALID`); a store that cannot
answer while authenticating is a 503 naming it. Every 500 is logged with its cause
(`writeInternal`), every JSON body is capped at 1 MiB with unknown fields refused (`readJSON`),
non-UUID path ids are 400 naming the kind (`pathUUID`), a content-addressed record id is checked
against its own lowercase-hex-sha256 shape rather than a UUID's (`pathRecordID`), and the
unauthenticated `POST /v1/launcher-credentials` is rate limited per source address (see
`BROKER_TRUSTED_PROXY_HEADER` above) and per named operator — the per-operator bucket keys on the
request body's own `operator` field, so an attacker naming a specific victim operator repeatedly
can still lock out that operator's launcher logins at a low rate; this is inherent to a
per-operator limit on an unauthenticated route and is an accepted risk, not a bug. Read `routes()`
for the current, authoritative route list.

`internal/broker/record` implements the credential-request record every human decision turns
on: `Body.Canonical()` renders the contract's fixed `\n`-terminated line format (the request
object verbatim, the approver, the enrollment's tab-separated kind, runtime id and operator, plus
a pod's slot as a fourth field only when it has one, lifetime, rules version, expiry, and the
machine-login code or `-`), `Body.ID()` is the lowercase-hex SHA-256 of that canonical form — the
record's own content-addressed id, which a slotless record keeps byte for byte — and `ParseBody`
is `Canonical`'s exact inverse, refusing any stored body that would not reproduce itself
byte-for-byte and any fourth enrollment field that is not a pod's valid slot. `VerifyRequestObject`
enforces the requester's signed request object end to end (single ES256 JWS, `typ`
`agent-secrets-request+jwt` — disjoint from the per-call proof's `agent-secrets-proof+jwt`, each
verifier refusing the other's — embedded P-256 JWK, `iss` equal to that JWK's own thumbprint, `aud`
equal to `BROKER_PUBLIC_URL`, `iat`/`exp` within skew and a 600-second cap, a `reason` of at most
400 runes with bidi/zero-width categories refused, and `authorization_details` either every entry
`agent_secret` or exactly one `launcher_credential` entry naming a valid hostname and an optional
`[a-z0-9-]{1,64}` service).
`Body.ApproverLogin(login)` is the one approver comparison: a record's approver is resolved when
the record is created (an approval rule's `login:<name>`, the requesting enrollment's operator for
`approver: operator`, or a machine login's `login_hint`), and every decision and every chain
re-check compares the canonical lowercase login against it, a decision recording the canonical
login it returns. `record.ChainVerifier` (`chain.go`, built per record kind by
`store.Store.ChainVerifier`, so a record of one kind never backs the other's credential) is
what "every release re-verifies the whole chain" means in code: given a record id it re-fetches
the stored body, confirms it still reproduces its own id, re-verifies the embedded request object
as of the record's own creation time (not now — a request object's ~10-minute `exp` is long past
by the time anything built from it is used again; this proves provenance, not freshness), and
requires exactly one terminal decision event, an `approved` one whose login is the record's
approver (the broker writes at most one; `credential_request_decision`'s partial unique index holds
it to that). It is called on every secret-value release (`requests.Machine.Values` and
`reuseLiveGrant`, via `VerifyChain`) and on every launcher-credential authentication
(`enroll.Service.AuthenticateLauncher`), so a grant or launcher-credential row with no approved
record behind it releases nothing. The approval itself proves who approved, as Dispatch reported
them: there is no approval signature to re-check, so a principal that can write the broker's
database can also write a passing record and event. That boundary is the database's own access
control, as it already was for grant rows.

`internal/broker/requests.Machine` is the `agent_secret` request state machine. `Create` verifies
the caller's signed request object (`iss` must be the requesting enrollment's own key, no
`login_hint` — a session never names its own approver, only the rules do), first checks for a
still-live grant covering the exact same name set (`reuseLiveGrant`: no new request, no new record,
as long as the current rules still allow it and the grant's whole chain still verifies), then
evaluates the rules per name: any `deny` denies the whole request with no record written at all; a
name no rule mentions at all aborts the whole call with `rules.ErrUnknownSecret` (`400
UNKNOWN_SECRET`, per the AGENTC-393 overview document) instead of being folded into an ordinary
`deny` decision — no request row is written either, matching the "at record time" wording; a name
needing approval that names a *different* approver than an already-approval-needing name in the
same request is refused `400 MIXED_APPROVERS`; when every name is decided (`granted`/`denied`) with
nothing pending, the request and, if granted, its grant are written with no record; a request
needing approval writes the request row and a `credential_requests` record together, in one
transaction serialized by an advisory lock keyed on the enrollment and the sorted name set, so an
identical concurrent request coalesces onto the same record (`coalesced: true`) instead of writing
a second one. Each of those write transactions first locks the requesting enrollment `for share`
while it is live, before any request or grant row, so a request racing the sweep or a revoke
writes nothing on an enrollment that ended after `Create` first read it (`401 PROOF_INVALID`, as
for one that had ended before). A pending request leaves `pending` without a human only through
`store.EndPendingRequests` (the session's cancel, the sweeper's expiry, an enrollment's end): one
statement moves the request rows and writes each one's audit row and its record's terminal event.
`ApplyDecision` decides a pending record on the deciding human's login — approve
mints the grant while the requesting enrollment is still live, deny denies it — re-deriving the
record's id, refusing any login but the record's approver whatever the record's state
(`record.ErrNotApprover`, `403 NOT_APPROVER`), re-verifying its embedded request object, and
writing the decision event (which records the deciding login), the request transition and the
audit row in one transaction; a non-pending record, and one past its expiry that the sweeper has
not yet expired, is `409 RECORD_TERMINAL` for its approver (a duplicate or late decision changes
nothing) — but a record past its expiry, whether the sweeper has recorded it expired or not,
answers with a message saying it expired before its approver acted (`requests.ErrExpired`), never
that it was decided. An `agent_secret` record is pending while its request is: `GET /v1/pending`
(`PendingForApprover`) lists it only then, and `GET /v1/credential-requests/{id}` (`ReadRecord`)
reads it as pending only then; a decided record's terminal event names the decision, and a request
cancelled with no cancelled event on its record, the shape an ended enrollment's requests had
before `endEnrollment` wrote one, reads as `cancelled` from its request row. A machine login is
pending while it carries no terminal event. `Values` releases a
live grant's inject-delivery values, re-checking the enrollment, the
grant, its whole approval chain (`VerifyChain`), and — when the rules changed since the grant was
decided — that the current rules still allow every granted name (`stillAllowed`: a name the rules no
longer carry, deny, or now want approved that was granted automatically, all refuse); a name is
released only when both its delivery frozen at grant time and its current delivery are `inject`,
else withheld in `proxy_only`; a source missing from the secrets store is `404 SECRET_NOT_IN_STORE`.
`RevokeGrant` lets a session end only its own grant (session proof); `RevokeByApprover` ends a grant
on a human's Dispatch login, allowed only when that login is the grant's approver or its
enrollment's operator (`mayRevoke`, else `403 NOT_APPROVER`); revoking an already-revoked grant is
a no-op, writing no second audit row. Audit rows never carry secret values: `audit()` takes only
`kind`, `enrollment_id`, `request_id`, an optional
`grant_id`, `actor` (`human:<login>`, `session:<enrollment id>`, `launcher:<credential id>`, or
`broker`), and a non-secret JSON `detail`. The granted value itself is read fresh from
`secrets.Reader` on release and never persisted.

`internal/broker/machine.Service` decides the other kind of credential request: a typed-code
machine login. `Login` verifies a machine's signed request object (`login_hint` required — the
approving operator's email — and exactly one `launcher_credential` authorization detail), mints an
eight-symbol confirmation code (`XXXX-XXXX`) and a separate opaque
`pending_id` the machine polls with, and writes the record plus its `machine_login_polls` row
(keyed by the pending id's own SHA-256 hash, never the raw capability). The operator's UI resolves a
pending login by that human-readable code alone (`LookupByCode` / `POST /v1/machine-logins/lookup`)
— never the machine's opaque pending id — and this is the *only* route that selects a machine
record (ruling 13: a direct link can never approve a machine login, only the typed code selects
it), so `code` is required and checked again on the decision itself, before the approver
(`400 CODE_REQUIRED` / `403 CODE_MISMATCH`). `ApplyDecision` locks the record's row (`for no key
update`, which an event insert's foreign-key check does not wait on), refuses any login but the
record's own approver (`403 NOT_APPROVER`) whatever the record's state, answers a record that
already carries a terminal event, or is past its `expires_at` before the sweeper has recorded it
expired, `409 RECORD_TERMINAL` before minting anything, as a decided `agent_secret` record
answers, so a second click, a concurrent one and a late one all get it — but a record past its
expiry, whether recorded expired by the sweeper or not, answers with a message saying it expired
before its approver acted (`machine.ErrLoginExpired`), never that it was decided, and, on
approval, mints the launcher
credential in the same transaction: bound to the request object's own key (its thumbprint and
embedded JWK, never a bearer token), with lifetime `BROKER_LAUNCHER_CREDENTIAL_SECONDS` counted
from the decision, so a crash between minting and recording the decision never orphans a
credential no decision names. Approving a login whose key already holds a live launcher credential
under another record (a machine that signed two logins with one key) is `409
KEY_HOLDS_LIVE_CREDENTIAL`, and that record stays pending. `Read` (the machine's own
poll, `GET /v1/launcher-credentials/{pending}`) answers only the record's state and, once issued,
the minted credential's id and `expires_at` — no token is ever returned; the credential is usable
only with proofs signed by the key the request object embedded. The broker has no renewal route:
past `expires_at` the machine logs in again, with a new key, a new code and a new human approval.

`internal/broker/enroll.Service.AuthenticateLauncher` is `proof.Verifier`'s `LookupLauncher` hook: a
launcher proof's `lid` claim resolves a live, unexpired `launcher_credentials` row and then
re-verifies its *entire* issuance chain through `record.ChainVerifier` before trusting it — a row is
never trusted on its own, so one inserted without a genuinely approved credential-request record
behind it never authenticates. `proof.Verifier.Verify` distinguishes a session proof (payload
carries `eid`) from a launcher proof (payload carries `lid`, never both or neither) but otherwise
checks the same things: `alg` exactly ES256, the embedded JWK's thumbprint matching the stored one,
signature, `iat` skew, `htm`/`htu`, and `jti` replay. `internal/broker/requests.Sweeper` is the one
thing that moves state without a human: every `BROKER_SWEEP_SECONDS` tick it first ends every
enrollment whose lease has lapsed (`enroll.Service.EndLapsed`: a gone pod, a box whose launcher
stopped renewing, a host session whose helper died), as a revoke ends one, so its grants are
revoked and its pending requests cancelled and dropped from the approver's list, with an
`enrollment.expired` audit row and one log line each; then it expires overdue pending
`agent_secret` requests (waking each one's owner through the Envoy wake seam) and overdue pending
machine logins. It reads fresh from Postgres every time, so a restart resumes exactly where the
rows are. Lapsed means what proof lookup means by not live (`lease_expires_at` no later than
Postgres's `now()`), so a session that keeps renewing is never ended, and rows a concurrent renew
or revoke holds are left to the next tick. Each ended enrollment's live grants are found through
`grants_enrollment_live` (migration 0008), so a backlog of lapsed enrollments costs one index
lookup each rather than a scan of `grants`.

Tests: `cd packages/envoy && go vet ./... && go test ./internal/broker/... ./cmd/broker/...
./cmd/agent-secrets/... ./cmd/agent-secrets-devrelay/...`. The Postgres-backed tests skip, rather
than fail, when `BROKER_TEST_DATABASE_URL` is unset (`t.Skip`, e.g. `internal/broker/store`,
`internal/broker/requests`); `packages/envoy/scripts/dev-postgres.sh` starts a local Postgres for
them, the same script `cmd/dispatch/README.md` documents for its own Postgres-backed tests, and
CI's `envoy-go` job (`.github/workflows/envoy-and-contracts.yaml`) points
`BROKER_TEST_DATABASE_URL` at the same server as `DISPATCH_TEST_DATABASE_URL`. Every client-facing
lane's own test mounts the real broker handlers (`api.Register` with real, Postgres-backed services)
rather than a hand-rolled fake standing in for broker behavior — `internal/broker/api/api_test.go`,
`internal/broker/e2e/e2e_test.go`, and `cmd/agent-secrets-devrelay/main_test.go` all wire real
`enroll.Service`/`requests.Machine`/`machine.Service` behind an `httptest.Server`;
`cmd/agent-secrets/main_test.go`'s own hand-rolled `fakeBroker` is the one sanctioned exception,
since it exists to test the CLI binary's own request-building and response-parsing logic, not
broker behavior. `packages/envoy/scripts/dev-broker.sh` and `cmd/agent-secrets-devrelay` (a stand-in
for Dispatch's relay: it sends the broker's UI routes the UI bearer and a login, as Dispatch does
when a signed-in human clicks Approve) run a whole local broker stack by hand for manual smoke
testing; neither ships in `docker/Dockerfile`, which builds exactly `envoy-listener`,
`envoy-dispatch`, `envoy-broker`, and `agent-secrets`. `dev-broker.sh` prints the exports a second
shell needs (`AGENT_SECRETS_URL`, `AGENT_SECRETS_UI_TOKEN`, `AGENT_SECRETS_APPROVER`, the helper's
`AGENT_SECRETS_HELPER_SOCK` and `AGENT_SECRETS_OPERATOR_FILE`, both in its workdir, where it writes
the operator file, and `DEV_BROKER_DIR`, the workdir itself). Each `dev-broker.sh` run creates
and drops its own isolated database, on the shared `dispatch-pg` container or, with
`DEV_BROKER_POSTGRES_URL` set, through `psql` on the server that URL names (no Docker), so
concurrent instances never see each other's enrollments, requests or grants, and listens on the
port its own `cmd/broker` binds and logs (`BROKER_LISTEN_ADDR=127.0.0.1:0`; AGENTC-833), so
concurrent instances can never collide on a shared port either.
`dev-broker.test.sh` proves both kinds of isolation with fakes (no real Postgres or network) and
runs in CI's `envoy-go` job.

`.github/workflows/release-envoy-listener.yaml` runs on a `main` push that touches a file the
image builds from, and by hand (`workflow_dispatch`) from any branch. Every run builds,
smoke-tests and pushes `ghcr.io/sjawhar/legion/envoy:<commit sha>`, labelled
`org.opencontainers.image.revision` with that sha. Its `release` job (moving `:latest`, the
`legion-envoy-v*` tag and the GitHub release) runs only on `refs/heads/main`, so a branch
dispatch publishes one immutable image, for a dev slot to pin before merge, and moves no tag.

`.github/workflows/release-envoy-listener.yaml`'s `legion-envoy-v*` release also ships
`cmd/agent-secrets` and the host helper `cmd/agent-secrets-helper` (AGENTC-393): each of
`agent-secrets-amd64.tar.gz` and `agent-secrets-arm64.tar.gz` wraps `bin/agent-secrets` and
`bin/agent-secrets-helper` in one top-level `agent-secrets/` directory — mise's `github:`
backend auto-strips exactly one leading directory, so the installed tree still ends up
`bin/agent-secrets`, `bin/agent-secrets-helper`, the layout its installer expects; a bare
`bin/...` top level would itself be the directory mise strips. The release job builds them, once
it has decided the tag, so it can stamp that tag into both, and attests the two tarballs; the
build job builds only `legion-envoy-<arch>.tar.gz` (envoy-listener alone). This is the release a
host installs both binaries from (AGENTC-834's dotfiles Plan B).

