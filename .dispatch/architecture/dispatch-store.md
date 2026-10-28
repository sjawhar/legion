---
title: Postgres store and migrations
parent: dispatch-server
depends_on: [dispatch-auth]
paths: [packages/envoy/internal/dispatch/store, packages/envoy/internal/dispatch/store/migrations, packages/envoy/internal/pgmigrate]
---
The schema every other package reads and writes through: ordered migrations applied in one transaction each at boot, the pool, and the session and people rows — which is why it imports `auth`. `internal/pgmigrate` holds the runner's rules, which the secrets broker's runner shares: the whole migration set is refused before anything applies when two files share a version or a file is misnamed or unreadable, and every migration's lock waits are bounded at five seconds. `internal/pgmigrate` reads migration and census SQL as Postgres 18's lexer does.

The schema also announces the changes the background loops wake on: `embeddings_enqueue` notifies `dispatch_embeddings` when it writes a row (0136), a row trigger on `refs` notifies `dispatch_excerpts_owed` for each document mention left owed an excerpt (0138), `delivery_settings_changed` notifies `dispatch_delivery_settings` for a settings change other than freshness (0141), and every event commit notifies `dispatch_events` (0107). The loops' own state is three additions: `reference_excerpt_backoff` (0137), each failing document's version, failures in a row and next attempt; `suggestion_outcome_cursor` (0139), the one row holding the highest event id the outcome sweep has judged by; and `write_suggestions.swept` (0140), whether a pass has judged a suggestion against all its events, with the partial indexes `write_suggestions_unswept` and `write_suggestions_pending_artifact`. The pool's listener pool dials the held LISTEN connections, the background loops' shared wake listener among them.
