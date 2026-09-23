---
title: Legion daemon
parent: legion
depends_on: [contracts, envoy-listener, dispatch-server]
paths: [packages/daemon]
---
The current TypeScript daemon: webhook intake, reducers, the durable `LegionState`, root-process lifecycle, credential grants, resync and recovery. It spawns phase workers as headless Oh My Pi processes (tmux or Kubernetes) and runs the merge queue through the controller. The role prompts it hands each worker ship with the Oh My Pi extension (`packages/pi-envoy/roles`), and it refuses to boot unless the installed extension speaks its daemon-API contract version - a runtime coupling, not a build dependency, so it is not drawn as an edge. Stage 7 replaces this shipped daemon with the Go coordinator.
