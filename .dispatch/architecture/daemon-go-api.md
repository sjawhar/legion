---
title: Coordinator API and CLI
parent: daemon-go
depends_on: [daemon-go-state, daemon-go-supervision, daemon-go-identity]
paths: [packages/daemon-go/cmd/legion, packages/daemon-go/internal/api, packages/daemon-go/internal/config, packages/daemon-go/internal/daemon]
---
Stage 1 on `main` provides the `legion` command, configuration, boot and shutdown assembly, and state and health routes. Stage 2 adds claim and operator routes; Stages 3 and 4 complete the agent API and CLI commands. Its API compatibility with the language-neutral contracts is not a Go source dependency.
