# Legion

Autonomous development swarm using Oh My Pi agents. Root processes own issue trees, and the Legion daemon supplies durable state, credentials, and event routing.

## Architecture

Legion is event-driven. The daemon derives role and gate state from GitHub artifacts, records
root-process session locators, and publishes only the verdict changes each role needs. Root
processes run in tmux; phase workers are native Oh My Pi subagents in the issue workspace.

- **TypeScript daemon** — webhook intake, reducers, durable `LegionState`, root-process lifecycle,
  credential grants, resync, recovery, and human-gate backstops.
- **OMP extension** — injects the Legion tool and event delivery into active OMP sessions, provisions
  issue workspaces, and enforces phase-worker admission and recursion limits.
- **Skills** — guide the architect and sequential phase workers. Durable `.legion/<phase>.json`
  handoffs are the recovery source of truth.

## Tech Stack

- **TypeScript** on **Bun** runtime
- **citty** for CLI, **Bun.serve** for HTTP daemon
- **Oh My Pi extension** for Legion tools, role delivery, workspace provisioning, and phase workers
- **Biome** for lint/format, **tsc** for type checking, **Bun test** for tests
- **jj (Jujutsu)** for version control, **Linear** or **GitHub Issues** for issue tracking

## Commands

```bash
bun install                   # Setup
bunx biome check src/         # Lint
bunx tsc --noEmit             # Type check
bun test                      # Test
```

```bash
legion start <team> [-w path] # Start swarm
legion status <team>          # Check status
legion stop <team>            # Stop swarm
legion restart <team>         # Restart daemon, preserve worker sessions
legion teams                  # List cached teams
legion attach <team> <issue>  # Attach to worker
legion handoff write|read|message    # Workers: write/read structured handoff data on issue branch
```

## Version Control

**jj (Jujutsu), not git.** Changes auto-accumulate. Push directly.

| Task                | Command                            |
| ------------------- | ---------------------------------- |
| Status / Log / Diff | `jj status` / `jj log` / `jj diff` |
| Push / Fetch        | `jj git push` / `jj git fetch`     |

## WHERE TO LOOK

| Task                   | Location                                      | Notes                                     |
| ---------------------- | --------------------------------------------- | ----------------------------------------- |
| Add CLI command        | `packages/daemon/src/cli/index.ts`            | citty `defineCommand` pattern                  |
| Change Legion API      | `packages/daemon/src/daemon/api.ts`            | See @packages/daemon/src/daemon/AGENTS.md      |
| Change daemon state    | `packages/daemon/src/daemon/legion-state.ts`   | See @packages/daemon/src/state/AGENTS.md       |
| Add phase guidance     | `skills/legion-worker/SKILL.md`                | See @skills/AGENTS.md                          |
| Change architect loop  | `skills/legion-architect/SKILL.md`             | See @skills/AGENTS.md                          |
| Handoff ledger         | `.legion/` on issue branch                     | Committed phase output                          |
| Envoy event routing    | `packages/envoy/`                              | See @packages/envoy/AGENTS.md                  |
| Shared event contracts | `packages/contracts/`                          | See @packages/contracts/AGENTS.md               |
| Envoy OMP adapter      | `packages/envoy-omp-extension/`                | See @packages/envoy-omp-extension/AGENTS.md    |

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

**Phase roles:** architect → plan → implement → test → review → merge
**Retro:** runs after reviewer cleanup and before human approval.

**Labels:** `needs-approval`, `human-approved`, `legion-child`, `legion-backlog`

**Review signaling:** Native GitHub review API, tester status checks, and committed handoffs are
the phase-verdict artifacts. No lifecycle labels carry worker state.

**Testing gate:** Behavioral testing is mandatory after every implementation phase — both fresh implementation AND review-requested changes go through the tester before reaching the reviewer.

## Documentation

- Plans: `docs/plans/YYYY-MM-DD-<slug>.md`
- Learnings: `docs/solutions/<category>/<slug>.md`

> Many docs in `docs/plans/` and `docs/solutions/` predate the TypeScript rewrite and contain Python-era references. These are marked with `[HISTORICAL]` headers.
```
