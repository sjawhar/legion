---
title: Admission and workflow
parent: daemon-go
depends_on: [contracts, envoy-listener, dispatch-server]
paths: [packages/daemon-go/internal/admit, packages/daemon-go/internal/intake, packages/daemon-go/internal/workflow, packages/daemon-go/internal/classify, packages/daemon-go/internal/dispatch, packages/daemon-go/internal/notify, packages/daemon-go/internal/phase]
---
The issue-cap admission line, durable Dispatch and GitHub event intake, event classification, transition table, status and notice effects, and the Go Dispatch client. It advances phases from facts the coordinator observes; agents do not choose the next phase.
