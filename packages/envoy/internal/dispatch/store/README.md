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

The complete graph uses `refs.AllEdgesSQL`: citation and structural `graph_edges`, identity and recorded-delivery `graph_person_edges`, indexed-inbox `graph_inbox_edges`, identity-mention `graph_mention_edges`, and participation `graph_thread_edges`.
`graph_edges` keeps its definition and existing builds' result sets; identity references are indexed under unknown storage target kinds `person_mention`, `session_mention` and `role_mention` that old readers skip.
The complete union filters those storage kinds from its compatibility arm and maps them only through `graph_mention_edges`; none of these views stores or writes an edge.
`person_aliases(person, alias_kind, alias)` derives canonical logins with `user_threads_login`, raw user actor ids, and session ids only where a stored session actor's server-verified `owner` identifies the person.
Routes, claims and role holders do not prove person ownership, so there is no role alias.
Delivery reads the four word-carrying event types used by `sessionanswers`, their `published_destinations`, and sent message/comment delivery attempts; a recorded role topic identifies the role, not a session or a person.
`envoy-dispatch census --deliveries` independently compares the graph's recipient sets with those records for every comment, message and ask created in the last 30 days.

`graph_inbox_edges` derives delivery to people from assigned asks, the existing `user_threads` index and direct-message roots (broadcast copies included).
Its direct reply walk stops at depth 64 and guards visited ids; read marks, Clear and per-reply reads stay in their existing tables and are subtracted by the named read.
`inbox_shadow_control`, `inbox_shadow_latest` and `inbox_shadow_days` store observation counts only, written by the one serialized comparison function, never another copy of an inbox or relation.
The comparison checks all signed-in people, a superset of active users.

Identity addresses are `dispatch://people/<login>`, `dispatch://sessions/<id>` and `dispatch://roles/<name>`; session and role ids follow the existing delivery route grammar.
The existing reference indexer records their resolved target from the same extraction it uses for citations; people use `model.CanonicalLogin`, the same canonical identity `person_aliases` names.
The mention view also reads explicit comment mentions, message targets and recorded delivery resolutions, expanding a session to a person only through a verified session alias.
`envoy-dispatch census --mentions` independently compares source sets created, edited or resolved in the last 30 days in one read-only snapshot; exit 1 includes an empty recorded-mention set.
An old build re-indexing identity-address text drops these unknown records; unchanged bodies are not automatically revisited by the owed sweep.
After returning to the new build, run `envoy-dispatch rebuild-refs`, let the existing block-index reconciliation drain, then run the mention census.

`graph_thread_participation` projects existing `user_threads` authorship alongside recorded mentions, deliveries and direct authorship; `graph_thread_edges` exposes the resulting `person -> participates_in -> thread root` relation.
`dispatch_thread_ancestors` is the shared bounded, cycle- and owner-guarded root walk, including comment replies whose root is an ask.
`GET /api/v1/me/threads/shadow` compares authored/answered membership against the source graph and server-owned answers, mentions against recorded resolution and addresses against recorded delivery; each cause reports missing/unexpected counts separately from membership beyond `user_threads`.
Read marks do not affect membership or activity ordering, and no new path writes `user_threads`.
