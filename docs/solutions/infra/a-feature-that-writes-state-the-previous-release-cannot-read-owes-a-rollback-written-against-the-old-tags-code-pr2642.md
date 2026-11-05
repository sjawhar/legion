---
title: "A feature that writes state the previous release cannot read owes a rollback written against the old tag's own code"
category: infra
tags:
  - rollback
  - release
  - durable-state
  - deployment
date: 2026-11-04
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2642"
---

# A feature that writes state the previous release cannot read owes a rollback written against the old tag's own code

- **"Re-pin the previous image" is not a rollback for a feature that writes durable state the
  previous release cannot interpret.** Enumerate each new durable thing the feature writes — a new
  enum value in a checked column, rows of a new table, frames of a new wire type — and say what the
  earlier binary does with each, citing that binary's own files **at its tag**, not at the current
  head.
- **State the loss plainly, and give the drain that avoids it before the downgrade**, not after:
  what must be finished or pushed while the new release still runs for nothing to be lost.
- **Give the recovery SQL in a form whose second run is a no-op**, and say what a clean rerun
  prints, so an operator who cannot tell whether the first run landed can simply run it again.
- **A documented recovery path for otherwise unrecoverable state is a claim until it is executed.**
  Run it before it ships and name the tool that read the artifact back.

## Evidence

sjawhar/legion#2642 makes the Legion daemon release an idle Kubernetes issue pod, which writes
three things a previous release cannot read: a new `released` suspension source in a checked
column, rows of `workspace_bundles` holding a released pod's unpushed work, and the
`launcher_bundle` frames that carry them. `docs/kubernetes.md` grew a "Rolling back idle release"
section that names each, says what `legion-v19.0.0` does with it — citing
`internal/supervise/machine.go` and `internal/workflow/resume.go` at that tag — and states the loss
without hedging: every released pod's unpushed work is lost in the rollback unless the running
release pushes it first. The drain comes before the downgrade, and the SQL prints `UPDATE 0` and
`DELETE 0` on a second run.

The same section gives the operator the command that reads a kept bundle out of the database. The
architect's acceptance records that it was executed against a test Postgres before it shipped: the
bytes came back identical and `git bundle list-heads` read them.
