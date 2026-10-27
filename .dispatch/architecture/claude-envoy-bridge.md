---
title: Claude Code plugin
parent: legion
depends_on: [envoy-client, contracts]
paths: [packages/claude-envoy]
---
The Claude Code channel plugin that delivers Envoy and Dispatch traffic into Claude Code sessions. It dials NATS with the client's shared connection options (a ping every 10 s, so a frozen path is left within 30 s), runs the catch-up of missed messages (`POST /v1/replay`) on each reconnect, and registers as `plugin` `@sjawhar/claude-envoy` with the install's commit (the version directory Claude Code's plugin cache holds it under) as `plugin_version`, or `source` outside the plugin cache.
