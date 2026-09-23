---
title: Process supervision and runtimes
parent: daemon-go
depends_on: [envoy-listener]
paths: [packages/daemon-go/internal/runtime, packages/daemon-go/internal/stream, packages/daemon-go/internal/shim, packages/daemon-go/internal/shimwire, packages/daemon-go/internal/supervise, packages/daemon-go/internal/workspace, packages/daemon-go/internal/prompts]
---
The fenced role-claim state machine, task delivery, worker stream and shim, and the pluggable tmux and Agent Sandbox runtimes. It keeps an open phase's agent reachable through Envoy, suspends it when the phase ends, and resumes the same session on demand.
