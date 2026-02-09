# Legion

Autonomous development swarm using OpenCode agents. Workers implement Linear issues in isolated jj workspaces, coordinated by a controller daemon.

## Two-Layer Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ OpenCode Skills (markdown)                                  │
│  controller: polls Linear, dispatches workers via HTTP      │
│  worker: 7 workflows (architect/plan/implement/review/...)  │
│  ↕ HTTP API + stdin/stdout + env vars                       │
├─────────────────────────────────────────────────────────────┤
│ TypeScript Daemon (Bun)                                     │
│  cli/       citty CLI (start/stop/status/attach/teams)      │
│  daemon/    HTTP server, worker process management          │
│  state/     Issue state machine, decision logic             │
│  util/      Base62 short IDs                                │
└─────────────────────────────────────────────────────────────┘
```

Skills invoke TypeScript via: HTTP API (`/workers`), piped CLI (`src/state/cli.ts`), and environment variables. TypeScript never calls skills directly.

## Tech Stack

- **TypeScript** on **Bun** runtime
- **citty** for CLI, **Bun.serve** for HTTP daemon
- **@opencode-ai/sdk** for programmatic OpenCode interaction
- **Biome** for lint/format, **tsc** for type checking, **Bun test** for tests
- **jj (Jujutsu)** for version control, **Stream Linear** for issue tracking (skill-embedded MCP)

## Commands

```bash
bun install                   # Setup
bunx biome check src/         # Lint
bunx tsc --noEmit             # Type check
bun test                      # Test (172 tests)
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
| Add CLI command | `src/cli/index.ts` | citty `defineCommand` pattern |
| Change HTTP API | `src/daemon/server.ts` | See @src/daemon/AGENTS.md |
| Change state machine | `src/state/decision.ts` | See @src/state/AGENTS.md |
| Add worker workflow | `.claude/skills/legion-worker/workflows/` | See @.claude/skills/AGENTS.md |
| Change controller loop | `.claude/skills/legion-controller/SKILL.md` | See @.claude/skills/AGENTS.md |
| Modify issue types | `src/state/types.ts` | Shared by daemon + state |
| Worker process mgmt | `src/daemon/serve-manager.ts` | Spawns `opencode serve` |
| Port allocation | `src/daemon/ports.ts` | Sequential from base 13381 |

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
Triage ──┬──► Icebox ──► Backlog ──► Todo ──► In Progress ──► Needs Review ──► Retro ──► Done
         │                  ^           ^            ^               │
         │                  │           │            │               │
         ├──────────────────┘           │            └───────────────┘
         │   (already spec-ready)       │            (changes requested)
         └──────────────────────────────┘
                    (urgent + clear)
```

**Worker modes:** architect → plan → implement → review → retro → merge

**Labels:** `worker-done`, `worker-active`, `user-input-needed`, `user-feedback-given`

**Review signaling:** PR draft status (not labels) — draft = changes requested, ready = approved.

## Documentation

- Plans: `docs/plans/YYYY-MM-DD-<slug>.md`
- Learnings: `docs/solutions/<category>/<slug>.md`

> Many docs in `docs/plans/` and `docs/solutions/` predate the TypeScript rewrite and contain Python-era references. These are marked with `[HISTORICAL]` headers.
