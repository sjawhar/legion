---
title: Postgres store and migrations
parent: dispatch-server
depends_on: [dispatch-auth]
paths: [packages/envoy/internal/dispatch/store, packages/envoy/internal/dispatch/store/migrations, packages/envoy/internal/pgmigrate]
---
The schema every other package reads and writes through: ordered migrations applied in one transaction each at boot, the pool, and the session and people rows — which is why it imports `auth`. `internal/pgmigrate` holds the runner's rules, which the secrets broker's runner shares: the whole migration set is refused before anything applies when two files share a version or a file is misnamed or unreadable, and every migration's lock waits are bounded at five seconds. `internal/pgmigrate` reads migration and census SQL as Postgres 18's lexer does.

The store also holds what the related-item check reads and writes: each project's term statistics (`search_term_stats`, `search_term_totals`, kept up to date under the `search-term-stats` lease), one `related_checks` row per check, and the trigger that embeds an issue as its title and spec (migrations 0139 and 0140). A pass of that refresh applies what changed rather than counting every item again: triggers on `asks`, `comments`, `issues`, `messages` and `document_search` queue each item whose search text a write changed, with the vector it held before (`search_term_queue`, migrations 0157 and 0159), and a pass drops exactly the queued rows its own snapshot sees, which it writes beside the counts (`search_term_totals.snapshot`, migration 0158) so that a later pass can tell its own work from another writer's. The lease pool has one connection per leased loop, thirteen.
