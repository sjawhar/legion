---
title: Claude Code plugin
parent: legion
depends_on: [envoy-client, contracts]
paths: [packages/claude-envoy]
---
The Claude Code channel plugin that delivers Envoy and Dispatch traffic into Claude Code sessions. Its `dist/` bundles the envoy-client `dispatch` command, so a change there, such as `dispatch search` printing each ask hit's answer state, reaches Claude Code only with `dist/` rebuilt. It takes the session's messages from the listener's relay of the session's own durable consumer, asked for at every registration and acknowledged once Claude Code re-emitted the message (`packages/envoy/AGENTS.md`, "Session consumers"). It dials NATS with the client's shared connection options (a ping every 10 s, so a frozen path is left within 30 s), runs the core path's catch-up of missed messages (`POST /v1/replay`) only on a listener that answers `core`, and registers as `plugin` `@sjawhar/claude-envoy` with the install's commit (the version directory Claude Code's plugin cache holds it under) as `plugin_version`, or `source` outside the plugin cache.
