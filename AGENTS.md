# Legion

Autonomous development swarm using OpenCode agents. Workers implement issues in isolated jj workspaces, coordinated by a controller daemon. Supports Linear and GitHub Issues (via Projects V2) as backends.

## Architecture

The state machine provides **deterministic defaults + raw signals**. The controller skill
**decides whether to follow or override** them. Users customize behavior by modifying the
controller skill, not the TypeScript.

- **TypeScript daemon** — thin substrate: spawns processes, tracks health, computes
  deterministic session IDs, collects signals from issue tracker/GitHub PRs/workers, suggests actions.
  The suggestions are testable defaults, not policy.
- **Controller skill** — the customization point: reads suggested actions + raw signals,
  executes transitions, runs quality gates, handles edge cases. Users who want different
  workflows edit this file.
- **Worker skills** — execute specific workflow phases (architect, plan, implement, test, review,
  merge). Retro runs via `/legion-retro` on the implement worker session.

Skills invoke TypeScript via: HTTP API (`/workers`, `/state/collect`), and environment variables (controller only). Workers receive all context via dispatch prompts. TypeScript never calls skills directly.

## Tech Stack

- **TypeScript** on **Bun** runtime
- **citty** for CLI, **Bun.serve** for HTTP daemon
- **@opencode-ai/sdk** for programmatic OpenCode interaction
- **Biome** for lint/format, **tsc** for type checking, **Bun test** for tests
- **jj (Jujutsu)** for version control, **Linear** or **GitHub Issues** for issue tracking

## Commands

```bash
bun install                   # Setup
bunx biome check src/         # Lint
bunx tsc --noEmit             # Type check
bun test                      # Test (640 tests)
```

```bash
legion start <team> [-w path] # Start swarm
legion status <team>          # Check status
legion stop <team>            # Stop swarm
legion teams                  # List cached teams
legion attach <team> <issue>  # Attach to worker
```

## Version Control

**jj (Jujutsu), not git.** Changes auto-accumulate. Push directly.

| Task | Command |
|------|---------|
| Status / Log / Diff | `jj status` / `jj log` / `jj diff` |
| Push / Fetch | `jj git push` / `jj git fetch` |

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| Add CLI command | `packages/daemon/src/cli/index.ts` | citty `defineCommand` pattern |
| Change HTTP API | `packages/daemon/src/daemon/server.ts` | See @packages/daemon/src/daemon/AGENTS.md |
| Change state machine | `packages/daemon/src/state/decision.ts` | See @packages/daemon/src/state/AGENTS.md |
| Add worker workflow | `.opencode/skills/legion-worker/workflows/` | See @.opencode/skills/AGENTS.md |
| Change controller loop | `.opencode/skills/legion-controller/SKILL.md` | See @.opencode/skills/AGENTS.md |
| Modify issue types | `packages/daemon/src/state/types.ts` | Shared by daemon + state |
| Worker process mgmt | `packages/daemon/src/daemon/serve-manager.ts` | Spawns `opencode serve` |
| Port allocation | `packages/daemon/src/daemon/ports.ts` | Sequential from base 13381 |

## Conventions

- **Strict mode** — `strict: true` in tsconfig
- **Biome** — double quotes, semicolons, ES5 trailing commas, 100 char width
- **Imports** — `node:` prefix for builtins, `type` keyword for type-only imports
- **Interfaces** for object shapes, **types** for unions/aliases
- **No barrel files** — direct imports between modules (intentional, avoids circular deps)
- **Dependency injection** — daemon accepts `overrides` for testability
- **Tests** — co-located `__tests__/` dirs, Bun test runner (`bun:test`)

## Issue Lifecycle

```
Triage ──┬──► Icebox ──► Backlog ──► Todo ──► In Progress ──► Testing ──► Needs Review ──► Retro ──► Done
         │                  ^           ^            ^                             │
         │                  │           │            │                             │
         ├──────────────────┘           │            └─────────────────────────────┘
         │   (already spec-ready)       │            (changes requested)
         └──────────────────────────────┘
                    (urgent + clear)

**Worker modes:** architect → plan → implement → test → review → merge
**Retro:** invoked by resuming the implement worker session with `/legion-retro`

**Labels:** `worker-done`, `worker-active`, `user-input-needed`, `user-feedback-given`, `test-passed`, `test-failed`

**Review signaling:** PR draft status (not labels) — draft = changes requested, ready = approved.

**Testing gate:** Behavioral testing is mandatory after every implementation phase — both fresh implementation AND review-requested changes go through the tester before reaching the reviewer.

## Documentation

- Plans: `docs/plans/YYYY-MM-DD-<slug>.md`
- Learnings: `docs/solutions/<category>/<slug>.md`

> Many docs in `docs/plans/` and `docs/solutions/` predate the TypeScript rewrite and contain Python-era references. These are marked with `[HISTORICAL]` headers.
