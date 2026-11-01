---
title: "A finished role's own cleanup is what destroys the live role's work: probe outside the shared workspace"
category: legion
tags:
  - workspace
  - jj
  - phase-workers
  - recovery
date: 2026-10-25
status: active
module: packages/daemon/internal/runtime/sandbox
related_issues:
  - "INFRA-56"
  - "sjawhar/legion#2158"
---

# A finished role's own cleanup is what destroys the live role's work

Extends docs/solutions/legion/the-issue-workspace-outlives-your-turn-INFRA-31.md.

That note's destructive actor is a live role proving something deliberately. This one is quieter:
a role whose phase is over, answering one more question, and tidying up after itself.

- **Once your phase has completed, run nothing in `$LEGION_WORKSPACE` that writes.** Your phase
  ended; the next role is editing the same files right now. A question from the architect is
  answered from committed state, not from a probe applied to the tree.
- **When a question genuinely needs a probe, copy the tree outside the workspace and probe the
  copy.** A Go `-overlay` that rewrites only a copy of the file is the same trick inside one
  command, and the reviewer of this issue used it to drift a production construction without
  touching the workspace at all.
- **The destructive step is the cleanup, not the probe.** `jj restore <paths>` restores the paths
  from the parent commit — all of them, including every uncommitted edit the live role has made
  to those paths since you applied yours. `jj diff` looked right before you ran it and empty
  after; nothing warns you that half of what vanished was not yours.
- **Recover with `jj op restore <operation>`, and say so at once.** The operation log holds every
  state; `jj op log` names the restore you just made and the operation before it. Then message
  the live role on its topic with what you reverted, what you restored it to, and the fact that
  only it knows what it had in flight.

## Evidence (INFRA-56, sjawhar/legion#2158)

The planner, its phase complete, was asked for one more measurement. It applied a throwaway copy
of the implementer's seam to the shared workspace, ran its probe, and cleaned up with
`jj restore packages/envoy/internal/dispatch/api/suggestions.go packages/envoy/internal/dispatch/api/server.go`
— by then those two files held the implementer's own step-1 edits, and the restore took them with
its own. It noticed within a minute, undid it with `jj op restore ee02a040c9b7`, and told the
implementer on its role topic to verify the tree against its own notes, which it did before
building on it. Nothing was lost, and the implementer's next commit carried the right content;
the same cleanup unnoticed for ten minutes would have been committed as the live role's work.
