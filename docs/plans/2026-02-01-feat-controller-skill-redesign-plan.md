---
title: "feat: Redesign Controller Skill for Decision-Making Architecture"
type: feat
date: 2026-02-01
---

# Redesign Controller Skill for Decision-Making Architecture

## Overview

Redesign the Legion controller skill from a procedural flowchart to a declarative decision-maker. The new controller defines affordances, information sources, goals, and priorities—then reasons from those primitives rather than following rigid steps.

The controller is woken by the daemon when decisions are needed, polls state, makes decisions, dispatches workers/subagents, and exits. Fresh context each iteration.

## Problem Statement / Motivation

The current controller skill (`legion-controller/SKILL.md`) follows a linear 8-step flowchart:
1. Get Linear state
2. Get tmux state
3. Identify work
4. Spawn workers
5. Clean up zombies
6. Merge completed
7. Write heartbeat
8. Exit

This approach has limitations:
- **Rigid priority**: Can't adapt to message urgency or blocked workers
- **No question handling**: No mechanism to help blocked workers via Oracle
- **No review pipeline**: Workers complete without review checkpoint
- **No grooming**: Backlog epics never get broken down
- **Limited reasoning**: Steps are prescriptive, not goal-oriented

The new design treats the controller as an LLM that reasons about goals given affordances and observations.

## Proposed Solution

Rewrite `legion-controller/SKILL.md` with:

1. **Declarative structure**: Define what controller CAN do, not what it MUST do in order
2. **Priority-based processing**: Messages → Reviews → Health → New work → Blocked workers → Grooming
3. **Phase-aware dispatch**: Track issue phase (planning → implementing → reviewing → retro → done)
4. **Worker specialization**: Planner, Implementer, Reviewer, Retro workers with distinct responsibilities
5. **Session resumption**: Resume Implementer sessions to maintain context across PR comments
6. **Goal statement**: Keep work moving, respect WIP, minimize user blocking

### Worker Architecture

| Mode | Linear Status | Responsibility | Outputs |
|------|---------------|----------------|---------|
| **plan** | Todo | Research → Plan → Deep research → Revise → Plan review → Finalize | Posts plan to Linear comment, adds `worker-done` |
| **implement** | In Progress | Read plan → TDD with subagents → Analyze → Open PR | PR created, adds `worker-done` |
| **review** | Needs Review | Deep review → Leave PR comments (blocking or approving) | PR comments, adds PR label THEN `worker-done` |
| **retro** | Retro | Implementer calls retro subagent to compound learnings | Writes to `docs/solutions/`, adds `worker-done` |
| **finish** | Retro→Done | Merge PR (resolve conflicts, fix CI) → Merge workspace → Clean up → Close issue | Issue closed, workspace removed |

### Skill Architecture (Router Pattern)

Instead of seven separate skills, we use the **router pattern**:

```
legion/
├── controller/
│   └── SKILL.md              # Custom: Core orchestration (this plan)
├── worker/
│   ├── SKILL.md              # Router: Essential principles + mode routing
│   ├── workflows/
│   │   ├── plan.md           # Invokes superpowers:writing-plans
│   │   ├── implement.md      # Invokes superpowers:test-driven-development
│   │   ├── review.md         # Invokes compound-engineering:workflows:review
│   │   ├── retro.md          # Invokes compound-engineering:workflows:compound
│   │   └── finish.md         # Merges PR, cleans up workspace, closes issue
│   └── references/
│       └── completion.md     # Label conventions, completion protocol
├── oracle/
│   └── SKILL.md              # Deferred: Evidence lookup (see Roadmap)
└── groomer/
    └── SKILL.md              # Deferred: Epic breakdown (see Roadmap)
```

**Why router pattern:**
- Shared principles enforced (all modes add `worker-done` label)
- Single deployment unit to version, test, install
- Token efficient (references loaded only when needed)
- Controller dispatches one skill with mode parameter

**Controller dispatches workers like:**
```bash
claude -p "Use legion-worker skill in plan mode for issue $ISSUE_ID"
claude -p "Use legion-worker skill in implement mode for issue $ISSUE_ID"
```

### Essential Principles (in worker SKILL.md)

These rules apply to ALL worker modes and cannot be skipped:

1. **Always add `worker-done` label** when exiting successfully
2. **Read issue context from Linear** before starting work
3. **Work in the jj workspace** at `$WORKSPACE_DIR`
4. **Post outputs to Linear** (plans, session IDs, status updates)
5. **For review mode**: Add `worker-approved` OR `worker-changes-requested` label

### Skill Summary

| Skill | Type | Purpose |
|-------|------|---------|
| **legion-controller** | Custom | Orchestration, phase transitions, label detection |
| **legion-worker** | Router | Unified worker with plan/implement/review/retro modes |
| **legion-oracle** | Deferred | Answer blocked worker questions (see Roadmap) |
| **legion-groomer** | Deferred | Epic breakdown (see Roadmap) |

### Session Resumption

Implementer sessions are resumed (not fresh-started) when addressing PR comments or running retro. This preserves context across the review cycle.

**Session ID:** `uuid5(project_id, issue_id:mode)` - deterministic, no storage needed. `--session-id` creates session if it doesn't exist.

```python
import uuid

# Linear project ID as namespace (it's already a UUID)
project_uuid = uuid.UUID(LINEAR_PROJECT_ID)

# Deterministic session ID = uuid5(namespace, name)
session_id = uuid.uuid5(project_uuid, f"{issue_id}:{mode}")

# Examples:
# ENG-21:implement → always same UUID
# ENG-21:review → different UUID (different mode)
# ENG-22:implement → different UUID (different issue)
```

**Controller spawns workers with:**
```bash
SESSION_ID=$(python3 -c "import uuid; print(uuid.uuid5(uuid.UUID('$LINEAR_PROJECT_ID'), '$ISSUE_ID:implement'))")
claude --session-id "$SESSION_ID" -p "Use legion-worker in implement mode for $ISSUE_ID"
```

**Resumption uses the same computed ID:**
```bash
# Same inputs = same session ID = resumes existing session
claude --session-id "$SESSION_ID" -p "Continue addressing PR comments"
```

**Why UUIDv5:**
- Deterministic (same inputs → same UUID)
- No storage or retrieval needed
- Namespace (project) + name (issue:mode) = unique per context
- Standard UUID format accepted by `--session-id`

### Information Sources (Read)

| Source | Access Method | Provides |
|--------|---------------|----------|
| Linear issues | `mcp__linear__list_issues` | Status, labels, issue IDs |
| Linear statuses | `mcp__linear__list_issue_statuses` | Which statuses are active (Todo, In Progress, etc.) |
| Worker completion | Linear label `worker-done` | Issue ready for next action |
| User feedback | Linear label `user-feedback-given` | Human answered a question |
| Review outcome | GitHub PR labels via `gh pr view --json labels` | `worker-approved` or `worker-changes-requested` |
| tmux windows | `tmux list-windows -t legion-<SHORT_ID>-workers` | Which workers are running |
| Worker session files | Read `~/.claude/projects/-path-to-workspace/*.jsonl` | Blocked state, current question |
| Blocked workers | Session file has pending AskUserQuestion | Worker needs input (escalate to user) |
| Session IDs | Computed via UUIDv5(project_id, issue_id:mode) | Session to resume (no storage) |

**Note:** Linear stays simple (status + `worker-done` label). Review outcome tracked on GitHub PR labels.

### Affordances (Write)

| Action | Method | When |
|--------|--------|------|
| Update Linear status | `mcp__linear__update_issue` | Phase transitions |
| Update Linear labels | `mcp__linear__update_issue` | Set `phase:*` labels, remove `worker-done` |
| Add Linear comment | `mcp__linear__create_comment` | Relay messages |
| Spawn worker (plan mode) | jj workspace add + tmux + `claude -p "Use legion-worker in plan mode"` | New Todo issue |
| Spawn worker (implement mode) | tmux + `claude -p "Use legion-worker in implement mode"` | Plan posted |
| Resume worker (implement mode) | tmux + `claude --resume <session-id>` | PR comments to address |
| Spawn worker (review mode) | tmux + `claude -p "Use legion-worker in review mode"` | PR opened |
| Spawn worker (retro mode) | tmux + `claude -p "Use legion-worker in retro mode"` | Reviewer approved |
| Send input to worker | `tmux send-keys -t <window>` | Relay user answer to blocked worker |
| Merge PR | `gh pr merge` | Retro complete |
| Merge workspaces | `jj new workerA@ workerB@ @` | After PR merged, cleanup |
| Escalate to user | Linear comment + `user-input-needed` label | Worker blocked on question |

### Controller Algorithm

**One iteration** (daemon restarts controller for next iteration):

```
1. RUN STATE SCRIPT
   - Python script collects all state (Linear, tmux, GitHub PRs, session files)
   - Outputs JSON of current state and suggested actions
   - Most logic is deterministic, doesn't need LLM reasoning

2. HANDLE USER INPUT LABELS
   - `user-input-needed`: skip issue, nothing to do yet (waiting for user)
   - `user-feedback-given`: relay feedback to the worker via tmux, remove label

3. PROCESS ISSUES (by status)
   - For each active issue, apply status-specific logic (see table below)
   - State script includes `has_live_worker` flag - skip dispatch if worker already running

4. ESCALATE BLOCKED WORKERS
   - Detect blocked workers via session files (pending AskUserQuestion)
   - Add Linear comment with question + `user-input-needed` label

5. WRITE HEARTBEAT
   - Touch ~/.legion/<SHORT_ID>/heartbeat

6. EXIT
```

### State Script (Python)

The controller runs a Python script that consolidates all state checking:

```python
# legion_state.py - outputs JSON, no LLM needed
{
  "issues": {
    "ENG-21": {
      "status": "Needs Review",
      "labels": ["worker-done"],
      "pr_is_draft": false,
      "has_live_worker": false,
      "suggested_action": "transition_to_retro"
    },
    "ENG-22": {
      "status": "In Progress",
      "labels": ["user-input-needed"],
      "has_live_worker": true,
      "suggested_action": "skip"  # user input needed
    }
  }
}
```

The LLM controller only reasons about edge cases or ambiguous situations.

**Status-Specific Logic:**

**Labels:**
- `worker-done` (Linear) - worker signals completion
- `worker-approved` / `worker-changes-requested` (GitHub PR) - review outcome
- `user-input-needed` (Linear) - skip issue, waiting for user

**Rules:**
- Reviewer adds PR label BEFORE `worker-done`
- Skip dispatch if `has_live_worker` is true (state script detects this)

| Status | Linear Label | PR Label | Action |
|--------|--------------|----------|--------|
| **Todo** | (none) | - | Dispatch planner (if no live worker) |
| **Todo** | `worker-done` | - | Remove label, move to In Progress, dispatch implementer |
| **In Progress** | (none) | - | No action (worker running or waiting for PR) |
| **In Progress** | `worker-done` | - | Remove label, dispatch reviewer |
| **Needs Review** | (none) | - | Dispatch reviewer (if no live worker) |
| **Needs Review** | `worker-done` | (none found) | Skip (PR label may not have propagated yet) |
| **Needs Review** | `worker-done` | `worker-changes-requested` | Remove labels (stay in Needs Review), resume implementer |
| **Needs Review** | `worker-done` | `worker-approved` | Remove label, move to Retro, resume implementer for retro |
| **Retro** | (none) | - | Resume implementer for retro (if no live worker) |
| **Retro** | `worker-done` | - | Remove label, dispatch finisher |
| **Done** | - | - | No action |

**Key simplifications:**
- Linear status IS the phase (no separate `phase:*` labels needed)
- Linear only needs one label: `worker-done`
- Review outcome on GitHub PR: `worker-approved` or `worker-changes-requested`
- Zombie cleanup handled by daemon, not controller (daemon monitors session file staleness + tmux window health)
- Finisher mode handles PR merge, merge conflicts, CI failures, and workspace cleanup

### Question Handling (Deferred)

**Note:** Oracle-based question handling is deferred to a separate design document. See Roadmap section.

For MVP, blocked workers will escalate directly to the user via Linear comment + `user-input-needed` label.

## Technical Considerations

### Session File Format

Worker sessions live at `~/.claude/projects/-path-to-workspace/*.jsonl`. Each line is a JSON object. Blocked state is detected by finding an `AskUserQuestion` tool call without a subsequent user response.

**Staleness threshold**: 15 minutes of no session file activity = potentially stuck worker. Verify via `tmux capture-pane` before intervention.

### Daemon Responsibilities (Not Controller)

The daemon handles mechanical monitoring that doesn't require LLM reasoning:
- **Session file staleness**: Monitor `~/.claude/projects/.../*.jsonl` modification times
- **tmux window health**: Check if worker windows are still running
- **Zombie cleanup**: Kill stuck workers, remove orphaned windows
- **Controller wake**: Restart controller periodically or on events

The controller only reasons about what to do—it doesn't actively monitor.

### tmux Answer Injection

```bash
# Escape special characters, send answer
tmux send-keys -t "legion-${SHORT_ID}-workers:${ISSUE_ID}" -l "${ESCAPED_ANSWER}"
tmux send-keys -t "legion-${SHORT_ID}-workers:${ISSUE_ID}" Enter
```

Use `-l` flag for literal strings. Escape single quotes by doubling them.

### WIP Limit

- Default: Unlimited (no limit)
- Configuration: `$LEGION_WIP_LIMIT` environment variable (optional)
- Scope: Only code workers count (Reviewer exempt as it's short-lived)
- Check before spawning: count tmux windows in workers session

### Workspace Persistence

Workspaces persist through the entire review cycle:

```
Todo → workspace created (jj workspace add)
    → Planner works in workspace
    → Implementer works in workspace
    → PR opened (code pushed from workspace)
    → Reviewer reviews (workspace unchanged)
    → Implementer resumes (same workspace)
    → Retro runs (reads from workspace)
    → Implementer compounds (same workspace)
    → PR merged
    → workspace merged (jj new <issue>@ @)
    → workspace deleted (jj workspace forget, rm -rf)
Done
```

This ensures:
- Context preserved across the review cycle
- No need to re-clone or re-fetch
- Session resumption works (same working directory)

### Error Handling

- **Linear API failure**: Log error, exit. Daemon will restart controller.
- **tmux command failure**: Log error, mark issue for retry next iteration.
- **jj workspace failure**: Log error, skip issue, continue with others.

Operations should be idempotent—running controller twice should be safe.


**Note:** Linear stays simple (only `worker-done`). Review outcome tracked on GitHub PR labels (since same user opens and reviews, can't use GitHub's native approve/request-changes).

**Critical ordering for reviewers:** Add PR label (`worker-approved` or `worker-changes-requested`) BEFORE adding `worker-done` to Linear. This ensures controller can always read the review outcome when it sees `worker-done`.

**PR-to-issue linking:** When PRs include the issue number in their name (e.g., "ENG-21-feature"), Linear automatically links them. Controller finds the PR via `gh pr list` filtered by the branch name pattern.

## Acceptance Criteria

### Core Functionality

- [ ] Python state script collects all state (Linear, tmux, GitHub PRs, session files) and outputs JSON
- [ ] State script detects live workers (`has_live_worker` flag) to prevent duplicate dispatch
- [ ] Controller skips issues with `user-input-needed` label (nothing to do yet)
- [ ] Controller relays feedback to worker when `user-feedback-given` appears, removes label
- [ ] Controller reads state from all information sources (Linear, session files, tmux, jj, PR labels)
- [ ] Controller processes priorities in order (messages → phase transitions → reviews → health → new work → blocked → grooming)
- [ ] Controller writes heartbeat before exit

### Status Transitions (Linear-Based)

- [ ] Controller queries Linear for issues with `worker-done` label
- [ ] Controller transitions Todo → In Progress when planner adds `worker-done`
- [ ] Controller transitions In Progress → Needs Review when implementer adds `worker-done`
- [ ] Controller checks GitHub PR for `worker-approved` / `worker-changes-requested` labels
- [ ] Controller stays in Needs Review when PR has `worker-changes-requested` (resumes implementer)
- [ ] Controller transitions Needs Review → Retro when PR has `worker-approved`
- [ ] Controller dispatches finisher when retro adds `worker-done`
- [ ] Controller removes `worker-done` from Linear after each transition
- [ ] Finisher handles: merge PR, cleanup workspace, close issue

### Session Resumption (Deterministic UUIDv5)

- [ ] Controller computes session ID via `uuid5(project_id, issue_id:mode)`
- [ ] Controller passes `--session-id` when spawning workers
- [ ] Controller resumes worker session (same computed ID) for PR comments
- [ ] Controller resumes worker session for retro (prompts to invoke worker skill with retro mode)
- [ ] Same inputs always produce same session ID (deterministic)

### Workspace Lifecycle

- [ ] Workspaces persist until issue is closed (not until PR merged)
- [ ] Controller merges workspace with `jj new` after PR merged
- [ ] Controller cleans up workspace directory after merge

### Blocked Worker Handling (MVP)

- [ ] Controller detects blocked workers via session file (pending AskUserQuestion)
- [ ] Controller escalates to user via Linear comment + `user-input-needed` label
- [ ] When `user-feedback-given` label added, controller relays answer to worker via tmux

**Deferred:** Oracle-based automated answering (see Roadmap)

### Skills (Router Pattern)

**Custom skills:**
- [ ] `controller/SKILL.md` - Core orchestration, label-based detection, phase transitions

**Router skill (unified worker):**
- [ ] `worker/SKILL.md` - Essential principles (labels, Linear, workspace) + mode routing
- [ ] `worker/workflows/plan.md` - Invokes planning skills, posts plan
- [ ] `worker/workflows/implement.md` - Invokes TDD skills, opens PR
- [ ] `worker/workflows/review.md` - Invokes review skills, leaves PR comments, adds PR label THEN `worker-done`
- [ ] `worker/workflows/retro.md` - Implementer calls retro subagent to compound learnings
- [ ] `worker/workflows/finish.md` - Merges PR, cleans up workspace, closes issue
- [ ] `worker/references/completion.md` - Label conventions shared across modes

**Deferred (see Roadmap):**
- [ ] `oracle/SKILL.md` - Automated question answering
- [ ] `groomer/SKILL.md` - Epic breakdown

### Error Handling

- [ ] Linear API failures logged and controller exits cleanly
- [ ] tmux failures logged, affected issues skipped
- [ ] jj failures logged, affected issues skipped
- [ ] All operations are idempotent

### Testing

- [ ] Test controller with no issues (no-op, exits cleanly)
- [ ] Test full phase cycle: Todo → Planning → Implementing → Reviewing → Retro → Done
- [ ] Test Implementer resume after Reviewer comments
- [ ] Test blocked worker (escalates to user via Linear comment + label)
- [ ] Test with WIP limit configured (skips new dispatch when at limit)
- [ ] Test workspace persists through review cycle
- [ ] Test PR merge and workspace cleanup at end

## Success Metrics

- Blocked workers escalated to user promptly
- No orphaned issues or zombie windows accumulate
- All completed work gets reviewed before merge
- Smooth phase transitions through the full cycle

## Dependencies & Risks

### Dependencies

- **Linear MCP server**: Must be configured and accessible
- **tmux**: Must be available on system
- **jj**: Must be available on system with workspace support
- **Claude Code**: Workers run Claude in tmux windows

### Risks

| Risk | Mitigation |
|------|------------|
| Session file format changes | Document expected format; add validation |
| Merge conflicts block progress | Finisher resolves conflicts; escalates if unresolvable |
| CI failures | Finisher retries or escalates |
| Race condition (PR label timing) | Reviewer adds PR label BEFORE `worker-done`; controller skips if label not found yet |

## References & Research

### Internal References

- Current controller skill: `/home/sami/legion/default/src/legion/skills/legion-controller/SKILL.md`
- Current worker skill: `/home/sami/legion/default/src/legion/skills/legion-worker/SKILL.md`
- Daemon implementation: `/home/sami/legion/default/src/legion/daemon.py`
- tmux utilities: `/home/sami/legion/default/src/legion/tmux.py`
- Worker design analysis: `/home/sami/legion/default/docs/plans/2026-01-31-worker-skill-design.md`

### Design Documents

- Controller brainstorm: `/home/sami/legion/default/docs/brainstorms/2026-02-01-controller-skill-design-brainstorm.md`
- Ralph swarm architecture: Referenced in brainstorm

### Open Design Work

The following require separate design documents:

1. **Task syncing daemon**: Sync `~/.claude/tasks/<session-id>/*.json` to Linear (as sub-issues or checklist comments)

**Resolved in this plan:**
- ~~PR comment parsing~~ → Reviewer adds `worker-approved` or `worker-changes-requested` labels
- ~~Session ID storage~~ → Deterministic UUIDv5 (no storage needed)
- ~~Compound skill integration~~ → Implementer calls retro subagent

## Roadmap (Deferred)

The following are out of scope for MVP and will be designed separately:

### Oracle Skill
**Purpose:** Automatically answer worker questions using codebase evidence before escalating to user.

**Why deferred:** Adds complexity. MVP works without it—workers escalate directly to user.

**Future design considerations:**
- Evidence sources and priority order
- Confidence model (when to answer vs. escalate)
- Answer injection mechanism via tmux

### Groomer Skill
**Purpose:** Break down epics into implementable features.

**Why deferred:** Not needed for single-issue workflow. Valuable once Legion handles multi-issue epics.

## Implementation Checklist

### Files to Create

**Custom Skills:**
```
~/.claude/skills/legion/
└── controller/
    └── SKILL.md              # Core orchestration (this plan)
```

**Router Skill (unified worker):**
```
~/.claude/skills/legion/worker/
├── SKILL.md                  # Essential principles + mode routing
├── workflows/
│   ├── plan.md               # Invokes planning skills, posts plan
│   ├── implement.md          # Invokes TDD skills, opens PR
│   ├── review.md             # Invokes review skills, leaves PR comments
│   ├── retro.md              # Invokes compound skills
│   └── finish.md             # Merges PR, cleans up workspace, closes issue
└── references/
    └── completion.md         # Label conventions, completion protocol
```

**Runtime directories** (created by controller as needed):
```
~/.legion/<SHORT_ID>/
└── heartbeat             # Controller heartbeat file
```

**Note:** Session IDs are computed via UUIDv5, not stored. See "Session Resumption" section.

### Files to Modify

- `/home/sami/legion/default/src/legion/skills/legion-controller/SKILL.md` - Complete rewrite
- `/home/sami/legion/default/src/legion/setup.py` - Add new skills to installation

### Key Implementation Steps

**State script:**
1. Create `legion_state.py` - collects Linear/tmux/GitHub/session state, outputs JSON with suggested actions

**Custom skills:**
2. Rewrite controller SKILL.md with status-based transitions and algorithm (uses state script)

**Router skill (unified worker):**
3. Create worker/SKILL.md with essential principles + mode routing
4. Create worker/workflows/plan.md (invoke planning skill + post to Linear)
5. Create worker/workflows/implement.md (invoke TDD skill + open PR)
6. Create worker/workflows/review.md (invoke review skill + leave PR comments + label ordering)
7. Create worker/workflows/retro.md (implementer calls retro subagent)
8. Create worker/workflows/finish.md (merge PR + resolve conflicts + handle CI + cleanup workspace + close issue)
9. Create worker/references/completion.md (label conventions)

**Infrastructure:**
10. Update setup.py to install new skill structure
11. Test full cycle: Todo → In Progress → Needs Review → Retro → Done
