---
title: Process supervision and runtimes
parent: daemon-go
depends_on: [daemon-go-identity]
paths: [packages/daemon-go/internal/runtime, packages/daemon-go/internal/stream, packages/daemon-go/internal/shim, packages/daemon-go/internal/shimwire, packages/daemon-go/internal/supervise, packages/daemon-go/internal/workspace, packages/daemon-go/internal/prompts]
---
Stage 2 adds the fenced role-claim state machine, task delivery, worker stream and shim, and the tmux runtime. Stage 3 plans workspace provisioning and role-prompt composition; Stage 4 plans the Agent Sandbox runtime. An open phase's agent remains reachable through Envoy and resumes as the same session on demand.
