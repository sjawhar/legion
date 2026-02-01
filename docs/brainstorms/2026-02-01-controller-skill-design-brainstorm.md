# Controller Skill Design Brainstorm

**Date:** 2026-02-01
**Status:** Ready for review
**Context:** Designing the Legion controller skill - the "brain" that coordinates workers

---

## What We're Building

The controller is a **decision-maker** that gets woken by the daemon when decisions are needed. It polls state, makes decisions, dispatches workers, and exits. Fresh context each iteration.

The controller does NOT actively monitor workers - the daemon handles mechanical monitoring (session file staleness, task sync). The controller only reasons about what to do.

---

## Key Decisions

### 1. Controller Structure

The skill defines:
- **Affordances** - what the controller can do
- **Information sources** - what it can observe
- **Goal statement** - keep work moving, respect WIP, minimize user blocking
- **Priority hints** - messages first, then reviews, then in-progress, etc.

The controller reasons from there. We don't prescribe a rigid flowchart.

### 2. Information Sources (Read)

| Source | How to Access | What It Provides |
|--------|---------------|------------------|
| Linear issues | `linear issue list`, `linear issue view` | Status, labels, comments, description |
| Worker session files | `~/.claude/projects/.../session.jsonl` | Last activity timestamp, blocked state, current question |
| Worker task lists | `~/.claude/tasks/<session-id>/*.json` | Progress on subtasks |
| tmux windows | `tmux list-windows -t <session>` | Which workers are running |
| jj workspaces | `jj workspace list` | Which code workspaces exist |

### 3. Affordances (Write)

| Action | How | When |
|--------|-----|------|
| Update Linear | `linear issue update`, comments | Status changes, relay messages |
| Spawn worker | Create jj workspace + tmux window + start Claude | New work to do |
| Send input to worker | `tmux send-keys -t <window>` | Answer question, provide context |
| Merge workspaces | `jj new workerA@ workerB@ @` | Workers complete |
| Dispatch subagent | Task tool | Oracle (answer questions), Groomer (break down epics), Reviewer (final check) |

### 4. Priority Order

When the controller wakes, it processes in this order:

1. **Read messages** - User feedback and agent-to-agent messages may influence all other decisions
2. **Handle blocked workers** - Workers waiting on questions (dispatch Oracle or escalate)
3. **Handle reviews** - Work waiting for review before completion
4. **Continue in-progress** - Ensure running workers are healthy
5. **Start new work** - Dispatch workers for unblocked Todo items (respect WIP limit)
6. **Groom backlog** - Break down epics into features (lowest priority)

### 5. Worker Handoff

When dispatching a worker, controller passes:
- **Issue ID** - The Linear issue identifier
- **Issue description** - The main content of the issue
- **Related issue IDs** - So controller can track parallel work

Worker fetches for itself:
- Comments on the issue
- Content of related issues
- Linked docs, specs, etc.

### 6. Worker Model

**Hybrid approach:**
- Worker owns the full TDD cycle (RED → GREEN → REFACTOR)
- Worker does self-review
- Controller dispatches a separate Reviewer worker before marking complete

### 7. Question Handling

When a worker is blocked on `AskUserQuestion`:
1. Controller reads the question from the session file
2. Controller dispatches **Oracle subagent** to try to answer
3. Oracle uses: spec/design doc, learnings system (`docs/solutions/`), related issues, codebase patterns
4. If Oracle answers with confidence → inject answer via `tmux send-keys`
5. If Oracle can't answer → escalate to user via Linear comment + `user-input-needed` label

### 8. WIP Limit

Configurable per-project (via Linear project metadata or environment variable).

Default: TBD (suggest 3-5 workers).

### 9. Iteration Lifecycle

```
Daemon wakes controller (periodic or event-triggered)
    ↓
Controller polls state (Linear, session files, tmux, jj)
    ↓
Controller makes decisions (dispatch, merge, escalate)
    ↓
Controller exits (fresh context next iteration)
```

---

## Why This Approach

1. **Decision-maker not monitor** - Keeps controller lightweight, fresh context each iteration. Daemon handles mechanical monitoring.

2. **Affordances not flowchart** - The controller is an LLM. Give it tools and goals, let it reason. Over-specifying leads to brittle behavior.

3. **Oracle before user** - Aligns with autonomy hierarchy: research → consult → decide → escalate. User is last resort.

4. **Hybrid worker model** - Workers have meaningful autonomy (full TDD cycle) but there's a checkpoint (reviewer) before completion.

---

## Open Design Work

### 1. Task Syncing Daemon

**What:** A non-LLM process that watches `~/.claude/tasks/<session-id>/*.json` and syncs to Linear.

**Questions to answer:**
- Sync as Linear sub-issues or comment checklists?
- Sync frequency?
- How to map task status to Linear state?
- How to handle task creation/deletion?

### 2. Oracle Subagent

**What:** A subagent that answers worker questions on behalf of the user.

**Questions to answer:**
- Evidence sources and priority order
- Confidence model (when to answer vs. escalate)
- How to inject answers back to workers
- Should Oracle explain its reasoning in Linear?

---

## Skill Files to Create

Based on this design:

```
~/.claude/skills/legion/
├── controller/
│   └── SKILL.md          # Controller skill (this design)
├── worker/
│   └── SKILL.md          # Worker skill (TDD cycle + self-review)
├── oracle/
│   └── SKILL.md          # Oracle subagent (answer questions)
├── groomer/
│   └── SKILL.md          # Break down epics into features
└── reviewer/
    └── SKILL.md          # Final review before completion
```

---

## References

- [Worker Skill Design](../plans/2026-01-31-worker-skill-design.md) - Hybrid approach analysis
- [Ralph Dev Swarm Design](../plans/2026-01-30-ralph-dev-swarm-design.md) - Architecture overview
- [Claude Code Task Files](#) - `~/.claude/tasks/<session-id>/*.json` format

---

## Next Steps

1. Design task syncing daemon (separate doc)
2. Design Oracle subagent (separate doc)
3. Implement controller skill based on this design
