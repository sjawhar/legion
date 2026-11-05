# Dispatch store

`migrations/` holds every `<version>_<name>.up.sql` boot applies in version order, with the `<version>_<name>.census.sql` each one declares for the pre-deploy census.
`<version>` is one of the numbers the set grew up with, each of whose files keeps the number it landed with, and for every migration added since the UTC moment its author created it, `YYYYMMDDHHMMSS` (`date -u +%Y%m%d%H%M%S`):
two pull requests open at once cannot both claim one timestamp, while both would claim the next number.
This file sits beside that directory rather than in it because the directory is embedded with `all:` and `pgmigrate.Load` refuses a set holding a file named anything but `<version>_<name>.up.sql`, `.down.sql` or `.census.sql`, which would refuse every boot.

Boot applies every version `schema_migrations` lacks, in version order, so a fresh database applies the whole set in that order, and a migration that merges below one a database has already applied still runs there, once.
A deployed database applies a migration when the release carrying it deploys:
if T2's release deploys before T1 merges, that database applies T2 then T1 while a fresh one applies T1 then T2.
So a migration must not depend on its order relative to one written in a concurrent pull request, another branch open at the same time;
one that needs another's effect is written after that one has merged.
`max(version)`, the `schema_version` `/healthz` reports, is the highest version applied, not proof that every version below it is applied, which is why the auto-deployer's rollback guard compares the whole set of recorded versions.

**A migration must leave the release before it able to read and write the schema.**
A Dispatch release rolls, so the old build and the new build run against this schema at once, and nothing at boot refuses a schema newer than the binary knows.
A change is therefore expand-then-contract across two releases:
the expanding migration adds, with a default or a backfill, and the release that ships it writes both shapes, while the migration that drops, renames or narrows ships no earlier than the next release, once no running build reads the old shape.
A migration that cannot be written that way is a stop-the-world migration, deployed with no server of the earlier release running, and that is an exception named in the pull request, not a default.

`../../../cmd/dispatch/README.md` ("Database migrations") has the rule with its examples, the naming and the identifiers a set is refused for, and what the census is held to.

## Derived graph

The graph is the union of `graph_edges`, `graph_person_edges` and `graph_mention_edges`, through `refs.AllEdgesSQL`. `graph_inbox_edges` is not in it.
`graph_edges` retains the citation and structural relations existing builds read; `graph_person_edges` derives `authored_by` and `delivered_to` without changing old builds' results or query plans, during a rollout or a rollback.
Neither view stores or writes an edge.
`person_aliases(person, alias_kind, alias)` is the one alias table, and it carries three kinds: `login`, the canonical person id `user_threads_login` derives; `actor_id`, the raw user actor id a record stored; and `session`, a session id.
A session alias is taken only from a stored session actor carrying `session_verified`.
A session actor's `owner` is always the credential's own — `bearerSessionActor` copies it from the authenticated token and never from the body — but its session id is the body's for every bearer except a broker-issued token, which the secrets broker binds to one session.
Without that guard a person's own token could write as any session and then read that session's deliveries as that person's.

**Deviation from the slice's spec, and who owns it: Sami Jawhar, with the rest of the person-graph work.**
The spec asks for roles in the alias table and for `delivered_to` edges that end at a person.
This table has neither, deliberately.
Dispatch records no durable fact that a role belongs to a person: a role's holder is resolved live by the listener at the moment of a forward (`outbox/role.go`), one role is held by different sessions over time, and an issue's route to a role records the role topic alone in `published_destinations`.
A person↔role alias would therefore have to be invented, and it would put every delivery a role ever received onto the page of everyone who ever held it.
So a role stays an address, not an identity: it is a node kind of its own (`refs.loadRoleNodes`), and a `delivered_to` edge to a role ends at that role node.
What a person received is read instead through their `login` and their proved `session` aliases, and a role delivery that actually reached a session is read there, because the resolution that reached it was recorded — `comment_deliveries.session_id` and `message_deliveries.session_id` hold the session a `role:<token>` target resolved to.
A role delivery that reached nobody (kept on the role's lane for the next holder) belongs to no person and is shown on the role node alone.
Delivery reads the four word-carrying event types used by `sessionanswers`, their `published_destinations`, and sent message/comment delivery attempts.
The person page resolves proved session aliases through canonical-login expression indexes before reading delivery attempts through their session/time indexes and recorded event topics through a partial GIN index.
Received nodes default to deliveries in the last 30 days; `since` (RFC3339) selects another lower bound without limiting authored nodes.
Role endpoints remain addressable in graph reads, including a delivery to a role alone.
`envoy-dispatch census --deliveries` independently compares the graph's recipient sets with those records for every comment, message and ask created in the last 30 days.

`graph_inbox_edges` is private current inbox membership, read only by the own-inbox query and comparison, not by `refs.AllEdgesSQL` or the references API.
Its `delivered_to` rows mean an ask currently assigned to a person, the newest write in a participating thread, or a direct/broadcast reply; they do not record historical delivery.
The query subtracts read state only; the reference side uses the Inbox route's own viewer-filtered predicate, `myRepliesUnread` and `unreadDirectRepliesCTE` independently.
The direct reply walk stops at depth 64 and guards visited ids; its root index uses the same `user_threads_login` normalization as other person-keyed tables.
Read marks, Clear and per-reply reads stay in their existing tables.
`inbox_shadow_control`, `inbox_shadow_latest` and `inbox_shadow_days` store observation counts only, written by the serialized background sweep, never another copy of an inbox or relation.
The comparison checks all signed-in people, a superset of active users; the HTTP read exposes only the caller's person row plus global totals.
The gate covers asks and replies only, not pending credential requests; a cutover must preserve the Credential requests section and its contribution to Needs you.

Identity references live in `identity_mentions`, which older writers never reconcile, and `graph_mention_edges` derives them from that table and from explicit comment mention records; none of these views stores or writes an edge.
Identity addresses are `dispatch://people/<login>`, `dispatch://sessions/<id>` and `dispatch://roles/<name>`; session and role ids follow the existing delivery route grammar.
The existing reference indexer records their resolved target from the same extraction it uses for citations; people use `model.CanonicalLogin`, the same canonical identity `person_aliases` names.
The mention view also reads explicit comment mentions and their recorded resolutions, expanding a session to a person only through a verified session alias.
A direct-message destination or broadcast copy is a delivery, not a mention; its body can still name an identity.
`envoy-dispatch census --mentions` reparses source bodies and current document content independently of reference rows, then compares complete source sets created, edited or resolved in the last 30 days in one read-only snapshot; exit 1 includes an empty recorded-mention set.
Reconciliation preserves a surviving mention's date and source event, so editing its block does not move an old mention up the person's page.
The outbox reads a comment creation event's own body for recipients and leaves pending, possibly delivered and refused attempts to the explicit delivery path.
Only an absent attempt or a known transport failure permits a mention fallback; a role forward to the same session takes precedence over that fallback.
