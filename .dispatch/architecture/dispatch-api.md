---
title: Dispatch API handlers
parent: dispatch-server
depends_on: [contracts, envoy-listener, dispatch-architecture, dispatch-auth, dispatch-docs, dispatch-events, dispatch-model, dispatch-pmdoc, dispatch-refs, dispatch-store, dispatch-text]
paths: [packages/envoy/internal/dispatch/api]
---
Every HTTP route body: issues, artifacts, asks, comments, messages, projects, and the assignee, rank, label and component-attachment writes, and the per-session list of what people wrote to an agent (`GET /api/v1/agents/{session_id}/answers`, `session_answers.go`), which every agent host reads on each model request and which reads the store's `SessionAnswers`. The largest package in the server (87 files) and the one every Dispatch feature touches. It resolves an agent's actor through the listener's OIDC and routing packages, hence the edge to `envoy-listener`. The edge to `contracts` is the wire contract rather than a Go import: `contracts/src/dispatch-api.ts` declares the response shapes these handlers produce and the web app consumes, and code generation covers only the event envelope, so there is no Go type to import.
