# MVP Implementation Planning Prompt

> Feed this prompt to a planning agent to create a detailed implementation plan.

---

## Context

You are planning the MVP implementation of **Ralph Dev Swarm**, an autonomous development system using Claude Code agents coordinated through Linear and jj.

Read the full design document first:
- `/home/sami/swarm/docs/plans/2026-01-30-ralph-dev-swarm-design.md`

## MVP Scope

Build the minimum system to prove the core loop works:

### In Scope

1. **Daemon** — Persistent process that:
   - Starts a tmux session
   - Launches Controller in a window
   - Runs a health loop (can skip Supervisor spawning for MVP — just log if heartbeat stale)

2. **Controller skill** (`ralph-dev-controller`) — Ephemeral Claude skill that:
   - Polls Linear for "In Progress" issues assigned to the project
   - For each issue without a running worker: spawns a worker in a new tmux window + jj workspace
   - Detects completed workers (tmux window gone or Linear status changed)
   - Merges completed workspaces back to main
   - Writes heartbeat file before exiting
   - Exits (Daemon restarts it for next iteration)

3. **One worker skill** (`ralph-dev-execute`) — Skill for implementing code:
   - Receives issue context via SessionStart hook
   - Implements the requested change
   - Runs tests
   - Pushes branch, creates PR (or updates Linear with PR link)
   - Self-terminates via Stop hook

4. **Hooks**:
   - `SessionStart`: Inject Linear issue details into worker prompt
   - `PostToolUse`: Run `jj status` to snapshot working copy
   - `Stop`: Clean up jj workspace, update Linear status

### Out of Scope (for MVP)

- Supervisor reasoning (just log staleness, don't spawn Supervisor yet)
- Other worker skills (plan, review, resolve, research)
- Local-first state complement
- Multiple projects simultaneously
- Sophisticated conflict resolution

## Constraints

- Use jj (Jujutsu) for version control, not git
- Use Linear as source of truth for task state
- Workers run in isolated jj workspaces
- Controller and workers are ephemeral (fresh context each run)
- Daemon is implementation-agnostic (could be Python, bash, or other)

## Deliverables

Create a detailed implementation plan that:

1. **Lists concrete files to create** with their locations
2. **Specifies the order of implementation** (what depends on what)
3. **Identifies unknowns or risks** that need investigation
4. **Defines "done" criteria** for the MVP (what proves it works)

## Output Location

Write the plan to:
`/home/sami/swarm/docs/plans/2026-01-31-mvp-implementation-plan.md`
