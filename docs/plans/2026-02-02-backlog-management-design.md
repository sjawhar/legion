# Legion Backlog Management Design

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Expand Legion to handle the full issue lifecycle, from raw intake through completion, with a persistent controller that builds context over time.

---

## Overview

This design adds:

1. **Persistent controller** - Builds context, handles triage directly, manages full lifecycle
2. **New statuses** - Triage and Icebox for intake management
3. **New `architect` worker mode** - Breaks down and specs Icebox items
4. **Renamed `merge` mode** - Previously called `finish`

**Key insight:** `worker-done` is the universal signal for "controller should take action." Items without it sit and wait.

---

## Status Flow

```
Triage ──┬──► Icebox ──► Backlog ──► Todo ──► In Progress ──► Needs Review ──► Retro ──► Done
         │                  ▲           ▲            ▲               │
         │                  │           │            │               │
         ├──────────────────┘           │            └───────────────┘
         │   (already spec-ready)       │            (changes requested)
         │                              │
         └──────────────────────────────┘
                    (urgent + clear)
```

| Status | Description | Who acts |
|--------|-------------|----------|
| Triage | Raw incoming items | Controller routes |
| Icebox | Needs architecture work | Controller pulls when capacity |
| Backlog | Holding area for spec work | Architect processes, controller moves forward |
| Todo | Ready for implementation planning | Planner processes |
| In Progress | Being implemented | Implementer processes |
| Needs Review | PR open, needs review | Reviewer processes |
| Retro | Capturing learnings | Retro worker processes |
| Done | Ready to merge and close | Merge worker processes |

---

## Controller (Persistent)

The controller is persistent to build context about ongoing work. This enables smarter triage decisions based on current priorities and in-flight work.

### Query Priority

Each iteration, the controller processes in order:

1. **Urgent Triage items** - Route high-priority items first
2. **`worker-done` items** (any status) - Process completions, move forward
3. **Non-urgent Triage items** - Route remaining triage items
4. **If capacity remains** - Pull from Icebox, move to Backlog, dispatch architect

**Implementation note:** Design the query to fetch all needed data in a single GraphQL call from Linear, then process in priority order locally.

### Triage Logic

When the controller sees a Triage item:

1. Read title, description, comments
2. Assess and route:
   - **Blocking bug / urgent + clear** → Todo (dispatch planner)
   - **Well-specified feature** → Backlog (will get prioritized)
   - **Vague / large / needs breakdown** → Icebox (will get architect)
3. Set appropriate priority

The controller's persistence lets it consider context: related in-flight work, recent patterns, overall capacity.

### Processing `worker-done`

When the controller sees an item with `worker-done`:

| Current Status | Action |
|----------------|--------|
| Backlog | Move to Todo, dispatch planner |
| Todo | Move to In Progress, dispatch implementer |
| In Progress | Move to Needs Review, dispatch reviewer |
| Needs Review (PR ready) | Move to Retro, dispatch retro worker |
| Needs Review (PR draft) | Keep in Needs Review, resume implementer |
| Retro | Move to Done, dispatch merge worker |

Always remove `worker-done` after processing.

### Pulling from Icebox

When the controller has capacity:

1. Select highest priority Icebox item
2. Move to Backlog
3. Dispatch architect

### Health Implications

Since the controller is persistent:
- Daemon must monitor for staleness carefully
- May need periodic context summarization
- Session resume strategy on crash/restart

---

## Worker Modes

| Mode | Dispatched for | Output | Adds `worker-done` |
|------|----------------|--------|-------------------|
| `architect` | Backlog (from Icebox) | Spec-ready issue(s) | Yes (or on children) |
| `plan` | Todo | Executable implementation plan | Yes |
| `implement` | In Progress | PR opened | No |
| `review` | Needs Review | PR marked ready/draft | Yes |
| `retro` | Retro | Learnings documented | Yes |
| `merge` | Done | PR merged, workspace cleaned | No |

### Architect Workflow

**Purpose:** Turn vague/large Icebox items into spec-ready work.

**Definition of Ready** (what "spec-ready" means):

- Clear problem statement (what and why)
- Acceptance criteria (testable conditions)
- Right-sized (fits in one PR, 1-3 days implementation)
- No unresolved blocking questions

Uses INVEST as a guide:
- **I**ndependent - No blocking dependencies
- **N**egotiable - Details refined during planning
- **V**aluable - Clear user/business value
- **E**stimable - Small enough to estimate
- **S**mall - Reasonable work unit
- **T**estable - Has acceptance criteria

**Workflow:**

```
1. Fetch Backlog issue (controller already moved it from Icebox)
2. Read title, description, comments
3. Assess size and clarity

4. If unclear requirements:
   → Add `user-input-needed` label
   → Post comment with specific questions
   → Exit (no `worker-done`)

5. If needs research:
   → Invoke oracle for guidance
   → Reassess with new context

6. If too big (won't fit in one PR):
   → Create sub-issues in Backlog, each with `worker-done`
   → Leave parent in Backlog (no label)
   → Exit
   (Controller will move children to Todo)
   (Linear auto-closes parent when children complete)

7. If spec-ready:
   → Add/refine acceptance criteria in description
   → Add `worker-done`
   → Exit
   (Controller will move to Todo)
```

**Sub-issue creation:**

When breaking down, architect creates Linear sub-issues:
- Title: Clear description of subset
- Description: Scoped requirements, acceptance criteria
- Status: Backlog
- Parent: Linked via Linear's parent field
- Label: `worker-done` (so controller picks them up)

### Plan Workflow

No changes from existing. See `skills/legion-worker/workflows/plan.md`.

### Implement Workflow

No changes from existing. See `skills/legion-worker/workflows/implement.md`.

### Review Workflow

No changes from existing. See `skills/legion-worker/workflows/review.md`.

### Retro Workflow

No changes from existing. See `skills/legion-worker/workflows/retro.md`.

### Merge Workflow (renamed from Finish)

No changes from existing behavior. See `skills/legion-worker/workflows/finish.md` (to be renamed to `merge.md`).

---

## Labels

Minimal label set:

| Label | Meaning | Added by | Removed by |
|-------|---------|----------|------------|
| `worker-done` | Worker finished, controller should act | Worker | Controller |
| `user-input-needed` | Blocked on human input | Worker | Controller |
| `user-feedback-given` | Human answered | Human | Controller |

**Key principle:** `worker-done` is the universal "ready for controller action" signal. Items without it are invisible to the controller (except Triage and Icebox pulls).

---

## Oracle

The oracle is a skill (not a worker mode) that provides research and guidance. It can be invoked by:

- **Controller** - For strategic/triage decisions
- **Architect** - For requirements research
- **Implementer** - For technical guidance
- **Planner** - For approach decisions

See existing `/oracle` skill usage in `skills/legion-worker/workflows/plan.md`.

---

## Parent Issue Handling

When architect breaks down a large issue:

1. Parent stays in Backlog with **no label**
2. Children created in Backlog with `worker-done`
3. Controller sees children's `worker-done`, moves them to Todo
4. Parent sits in Backlog, invisible to controller
5. Linear auto-closes parent when all children complete

**Why parent is safe from re-processing:**
- Controller only acts on `worker-done` items (for moving forward)
- Controller only pulls from Icebox (not Backlog) for new architect work
- Parent has no `worker-done`, so controller ignores it

---

## Implementation Tasks

### Task 1: Add new Linear statuses

Create in Linear:
- Triage
- Icebox

Verify existing:
- Backlog
- Todo
- In Progress
- Needs Review
- Retro (may need to create)
- Done

### Task 2: Update controller skill

Modify `skills/legion-controller/SKILL.md`:
- Make persistent (remove "exit after one iteration")
- Add triage logic (routing)
- Add Icebox pull logic
- Update query patterns for `worker-done` processing

### Task 3: Create architect workflow

Create `skills/legion-worker/workflows/architect.md`:
- Definition of Ready criteria
- Breakdown logic
- Sub-issue creation
- Oracle integration

### Task 4: Update worker skill routing

Modify `skills/legion-worker/SKILL.md`:
- Add `architect` mode
- Rename `finish` to `merge` in routing table

### Task 5: Rename finish workflow

Rename `skills/legion-worker/workflows/finish.md` to `merge.md`

### Task 6: Update daemon for persistent controller

Modify daemon to handle persistent controller:
- Health monitoring changes
- Context management
- Session resume on crash

### Task 7: Update state machine

Modify `src/legion/state/` to handle new statuses and transitions.

---

## Open Questions

1. **Controller context management** - How do we handle context window limits for a persistent controller? Periodic summarization? Session handoff?

2. **Icebox prioritization** - How does controller decide which Icebox item to pull? FIFO? Priority field? Age?

3. **Capacity detection** - How does controller know it "has capacity" to pull from Icebox? Based on active worker count? Time since last dispatch?
