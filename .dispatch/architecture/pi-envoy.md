---
title: Oh My Pi Envoy plugin
parent: legion
depends_on: [envoy-client, contracts, pi-shared]
paths: [packages/pi-envoy]
---
The Oh My Pi plugin every session loads (`@sjawhar/pi-envoy`): Envoy subscriptions and delivery, the native Dispatch tools, the `dispatch-first` context, and the four Dispatch and Envoy skills. It publishes the in-process interface in `pi-shared` that the Legion plugin claims its roles through.
