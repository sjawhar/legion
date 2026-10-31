---
title: Postgres store and migrations
parent: dispatch-server
depends_on: [dispatch-auth]
paths: [packages/envoy/internal/dispatch/store, packages/envoy/internal/dispatch/store/migrations, packages/envoy/internal/pgmigrate]
---
The schema every other package reads and writes through: ordered migrations applied in one transaction each at boot, the pool, and the session and people rows — which is why it imports `auth`. `internal/pgmigrate` holds the runner's rules, which the secrets broker's runner shares: the whole migration set is refused before anything applies when two files share a version or a file is misnamed or unreadable, and every migration's lock waits are bounded at five seconds. `internal/pgmigrate` reads migration and census SQL as Postgres 18's lexer does.

The store also holds what the related-item check reads and writes: each project's term statistics (`search_term_stats`, `search_term_totals`, rebuilt under the `search-term-stats` lease), one `related_checks` row per check, and the trigger that embeds an issue as its title and spec (migrations 0139 and 0140). The lease pool has one connection per leased loop, fourteen.
