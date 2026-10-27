---
title: Envoy client
parent: legion
depends_on: [contracts]
paths: [packages/envoy-client]
---
Shared TypeScript HTTP layer and `dispatch` command for every agent host: Dispatch config resolution, command execution, inbound envelope rendering. It also holds the NATS connection options every session plugin dials with (`nats-connection`: a ping every 10 s, a path dropped after two unanswered pings, so a frozen path is left within 30 s; reconnect forever, 2 s apart), and the session registration, whose `subscribe()` sends the plugin's npm package name and build as `plugin` and `plugin_version` on every registration.
