---
title: Postgres store and migrations
parent: dispatch-server
depends_on: [contracts, dispatch-auth, dispatch-model]
paths: [packages/envoy/internal/dispatch/store, packages/envoy/internal/dispatch/store/migrations, packages/envoy/internal/pgmigrate]
---
The schema every other package reads and writes through: ordered migrations applied in one transaction each at boot, the pool, and the session and people rows — which is why it imports `auth`. `internal/pgmigrate` holds the runner's rules, which the secrets broker's runner shares: the whole migration set is refused before anything applies when two files share a version or a file is misnamed or unreadable, and every migration's lock waits are bounded at five seconds. It also holds `SessionAnswers`, the one read of what people wrote that Dispatch delivered to a session (`GET /api/v1/agents/{session_id}/answers`), shared by the API and the outbox's tests so the route reads what the publisher records: a person's events whose `published_destinations` hold the session's own topic (`contracts.AgentSubject`, hence that edge), its sent message and comment deliveries, each item at its current words as a `model.SessionAnswer`.
