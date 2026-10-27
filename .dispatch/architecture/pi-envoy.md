---
title: Oh My Pi Envoy plugin
parent: legion
depends_on: [envoy-client, contracts, pi-shared]
paths: [packages/pi-envoy]
---
The Oh My Pi plugin every session loads (`@sjawhar/pi-envoy`): Envoy subscriptions and delivery, the `dispatch` command on every shell's `PATH`, the `dispatch-first` context, and the five skills every session with Dispatch reads (the Dispatch and Envoy skills, and `asd-ste100`). It publishes the in-process interface in `pi-shared` that the Legion plugin claims its roles through. It dials NATS with the client's shared connection options (a ping every 10 s, so a frozen path is left within 30 s), runs the catch-up of missed messages (`POST /v1/replay`) on each reconnect, and registers as `plugin` `@sjawhar/pi-envoy` with its package version as `plugin_version`.
