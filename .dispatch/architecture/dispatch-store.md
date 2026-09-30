---
title: Postgres store and migrations
parent: dispatch-server
depends_on: [dispatch-auth]
paths: [packages/envoy/internal/dispatch/store, packages/envoy/internal/dispatch/store/migrations, packages/envoy/internal/pgmigrate]
---
The schema every other package reads and writes through: ordered migrations applied in one transaction each at boot, the pool, and the session and user rows — which is why it imports `auth`. `internal/pgmigrate` holds the runner's rules, which the secrets broker's runner shares: the whole migration set is refused before anything applies when two files share a version or a file is misnamed or unreadable, and every migration's lock waits are bounded at five seconds.
