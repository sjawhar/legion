---
title: Oh My Pi Envoy plugin
parent: legion
depends_on: [envoy-client, contracts, pi-shared]
paths: [packages/pi-envoy]
---
The Oh My Pi plugin every session loads (`@sjawhar/pi-envoy`): Envoy subscriptions and delivery, the `dispatch` command on every shell's `PATH`, the `dispatch-first` context, and the five skills every session with Dispatch reads (the Dispatch and Envoy skills, and `asd-ste100`). It publishes the in-process interface in `pi-shared` that the Legion plugin claims its roles through. It takes the session's messages from the listener's relay of the session's own durable consumer, asked for at every registration and acknowledged once the session wrote the message (`packages/envoy/AGENTS.md`, "Session consumers"). It dials NATS with the client's shared connection options (a ping every 10 s, so a frozen path is left within 30 s), runs the core path's catch-up of missed messages (`POST /v1/replay`) only on a listener that answers `core`, and registers as `plugin` `@sjawhar/pi-envoy` with its package version as `plugin_version`.
