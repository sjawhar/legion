---
title: Admission and workflow
parent: daemon-go
---
Planned: the issue-cap admission line, durable Dispatch and GitHub event intake, classification, the transition table, status and notice effects, and the Go Dispatch client — `internal/admit`, `internal/intake`, `internal/workflow`, `internal/classify`, `internal/dispatch`, `internal/notify`, `internal/phase`. LEGION-219 (Stage 3) carries them; none exists on `main` yet, so this component has no paths. The planned workflow advances phases from facts the coordinator observes; agents do not choose the next phase.
