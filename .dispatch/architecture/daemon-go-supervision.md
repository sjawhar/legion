---
title: Process supervision and runtimes
parent: daemon-go
depends_on: [daemon-go-identity]
paths: [packages/daemon-go/internal/runtime, packages/daemon-go/internal/supervise, packages/daemon-go/internal/stream, packages/daemon-go/internal/shim, packages/daemon-go/internal/shimwire]
---
Stage 2 on `main` provides the fenced role-claim state machine and task delivery (`internal/supervise`), the worker stream and shim (`internal/stream`, `internal/shim`, `internal/shimwire`), and the pluggable runtime with its tmux implementation (`internal/runtime`). Stage 3 plans workspace provisioning and role-prompt composition in `internal/workspace` and `internal/prompts`; Stage 4 plans the Agent Sandbox runtime beside tmux, which stays a supported runtime (LEGION-206). An open phase's agent remains reachable through Envoy and resumes as the same session on demand.
