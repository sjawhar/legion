---
title: Coordinator API and CLI
parent: daemon-go
depends_on: [daemon-go-state, daemon-go-supervision, daemon-go-identity]
paths: [packages/daemon-go/cmd/legion, packages/daemon-go/internal/api, packages/daemon-go/internal/config, packages/daemon-go/internal/daemon]
---
Stages 1 and 2 on `main` provide the `legion` command (including `worker-shim` and `claims`), configuration, boot and shutdown assembly, the state and health routes, and the claim-registration and operator routes. Stages 3 and 4 complete the agent API and CLI commands. Its API compatibility with the language-neutral contracts is not a Go source dependency.
