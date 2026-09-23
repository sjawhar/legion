---
title: Admission and workflow
parent: daemon-go
paths: [packages/daemon-go/internal/admit, packages/daemon-go/internal/intake, packages/daemon-go/internal/workflow, packages/daemon-go/internal/classify, packages/daemon-go/internal/dispatch, packages/daemon-go/internal/notify, packages/daemon-go/internal/phase]
---
These Stage 3 packages are planned: the issue-cap admission line, durable Dispatch and GitHub event intake, classification, transition table, status and notice effects, and the Go Dispatch client. The planned workflow advances phases from facts the coordinator observes; agents do not choose the next phase.
