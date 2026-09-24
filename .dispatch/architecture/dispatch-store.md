---
title: Postgres store and migrations
parent: dispatch-server
depends_on: [dispatch-auth]
paths: [packages/envoy/internal/dispatch/store, packages/envoy/internal/dispatch/store/migrations]
---
The schema every other package reads and writes through: 43 ordered migrations applied in one transaction each at boot, the pool, and the session and user rows — which is why it imports `auth`.
